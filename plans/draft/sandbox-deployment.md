# Plan: Sandbox Execution + Kubernetes Deployment

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres is the system of record.**  
> All agent state, task queues, and lifecycle events remain DB-backed regardless of execution backend.

## Context

Maquinista currently runs as a bare binary on the host machine, using tmux to manage agent
processes. This works for a single developer machine but has hard limits:

- No isolation — a runaway agent can kill the host process or neighbours
- No resource enforcement — agents share host CPU/RAM with no per-agent quotas
- No horizontal scale — single host, single `TMUX_SESSION_NAME`
- No HA — binary crash kills all agents
- Deployment is manual (`git pull && make build && ./maquinista start`)

This plan does two things in one track:

1. **Introduce a `sandbox.Provider` abstraction** so the execution backend is swappable without
   touching orchestrator or bot logic. tmux stays the default; E2B (managed Firecracker) and
   Kubernetes + Kata (self-hosted Firecracker) slot in as backends behind the same interface.

2. **Deploy the maquinista stack to the Hetzner k3s cluster** (`brisakube`, managed via
   `terraform-hcloud-kube-hetzner`), with each agent running in a hardware-isolated Firecracker
   microVM via Kata Containers.

---

## Cluster Baseline (brisakube)

```
Control Plane:  1x cx23 (2 vCPU / 4 GB)
Workers:        2x cx33 (4 vCPU / 8 GB each)
Region:         nbg1 (Nuremberg)
K8s distro:     k3s v1.33 on openSUSE MicroOS
CNI:            Flannel (VXLAN)
Ingress:        nginx
TLS:            cert-manager + Let's Encrypt
Storage:        Hetzner CSI (cloud volumes)
GitOps:         ArgoCD v3 (apps namespace: brisaapps)
Runtime:        containerd + runc (default — no sandbox runtime today)
```

---

## Sandbox Runtime Evaluation

### Option A — Kata Containers with Firecracker (`kata-fc`)

Each agent pod runs in a dedicated Firecracker microVM. Kata injects a thin kernel and maps
container image layers as OverlayFS drives inside the VM — the same technique E2B uses
internally (https://e2b.dev/blog/scaling-firecracker-using-overlayfs-to-save-disk-space). The
base image (squashfs, read-only) is shared across all VMs; each VM gets a sparse ext4 upper
layer for COW writes, paying disk only for actual mutations. containerd's snapshotting handles
this automatically — no custom `overlay-init` scripting needed.

**Isolation**: hardware VM boundary per pod.  
**Boot**: ~125 ms cold, ~50 ms from snapshot.  
**Overhead**: ~30 MB RAM per VM.  
**Hetzner caveat**: requires `/dev/kvm` on the worker node. Hetzner cx-series VMs may or may
not expose KVM depending on the host; verify before committing to this path (see Phase 0).  
**Verdict**: primary target.

### Option B — Kata with QEMU (`kata-qemu`)

Same Kata layer, heavier VMM. Boot ~500 ms, overhead ~70 MB/VM. Also requires `/dev/kvm`.  
**Verdict**: fallback if Firecracker is incompatible with Hetzner virtualisation.

### Option C — gVisor (`runsc`)

Userspace kernel; no `/dev/kvm` required. Near-zero boot, ~10 MB overhead. The claude CLI and
git make many syscalls; gVisor ptrace mode adds a 2–5× overhead. Acceptable fallback.  
**Verdict**: use on nodes without KVM.

### Option D — E2B managed service

E2B runs Firecracker microVMs as a managed API. No cluster changes needed. Useful as a
stepping-stone before self-hosting, or as a permanent backend for dev/staging.  
**Verdict**: implement via the same `sandbox.Provider` interface; see Phase 4.

### Option E — runc (plain pods)

Namespace/cgroup isolation only. Acceptable for the maquinista control-plane pods (orchestrator,
dashboard) but not for agent code execution.  
**Verdict**: control-plane pods only.

### Decision Matrix

| Option | Isolation | Boot | RAM/agent | KVM required | Use for |
|--------|-----------|------|-----------|--------------|---------|
| kata-fc | VM (HW) | 125 ms | 30 MB | Yes | **Agent pods (primary)** |
| kata-qemu | VM (HW) | 500 ms | 70 MB | Yes | Agent fallback |
| gVisor | syscall | ~0 | 10 MB | No | No-KVM fallback |
| E2B | VM (HW, managed) | ~300 ms | — | No (managed) | Dev / managed path |
| runc | ns/cgroup | ~0 | ~0 | No | Control plane |

---

## Architecture Target

```
┌─────────────────────── brisakube (k3s) ──────────────────────────┐
│  Namespace: maquinista                                             │
│                                                                    │
│  Deployment: maquinista-orchestrator (runc)                        │
│    polls task queue → spawns agent Jobs via k8s API               │
│               │                                                    │
│               │ creates                                            │
│               ▼                                                    │
│  Job: agent-<id>  (runtimeClassName: kata-fc)                     │
│    image: maquinista-agent:latest                                  │
│    OverlayFS: shared squashfs base + sparse ext4 per-VM overlay   │
│    env: AGENT_ID, DATABASE_URL, BRANCH, WORKTREE_DIR              │
│                                                                    │
│  Deployment: maquinista-dashboard (runc)                           │
│    Next.js + Go API — nginx Ingress + TLS                          │
│                                                                    │
│  StatefulSet: postgres — Hetzner CSI PVC 10 Gi                    │
│                                                                    │
│  kube-system:                                                      │
│    DaemonSet: kata-deploy (installs kata binaries on agent nodes) │
│    RuntimeClass: kata-fc, gvisor                                   │
└────────────────────────────────────────────────────────────────────┘
```

Postgres is the system of record. Agent job pods connect via `DATABASE_URL`. The orchestrator
creates and watches k8s Jobs instead of tmux windows.

---

## Phase 0 — Pre-flight: KVM check

Gate before any kata work. SSH to each worker node:

```bash
ls /dev/kvm          # must exist
grep -E '(vmx|svm)' /proc/cpuinfo
```

Or run cluster-wide via a one-off Job:

```yaml
# brisakube/maquinista/kvm-check.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: kvm-check
  namespace: maquinista
spec:
  template:
    spec:
      tolerations:
        - operator: Exists
      containers:
        - name: check
          image: busybox
          command: ["sh", "-c", "ls -la /dev/kvm && echo KVM_OK || echo KVM_MISSING"]
          securityContext:
            privileged: true
      restartPolicy: Never
```

- KVM present → proceed with `kata-fc`
- KVM absent → use `gvisor` as `runtimeClassName`; skip kata-deploy DaemonSet

---

## Phase 1 — Sandbox Abstraction (no behaviour change)

New package: `internal/sandbox/`

### `internal/sandbox/sandbox.go`

```go
type Sandbox interface {
    Ref() string                               // stored in DB: tmux window ID, E2B sandbox ID, or k8s Job name
    Driver() sidecar.PtyDriver                 // stdin into the agent
    Tailer() sidecar.TranscriptTailer          // transcript event stream out
    IsAlive(ctx context.Context) bool
    Stop(ctx context.Context) error
}

type CreateOpts struct {
    AgentID      string
    WorkingDir   string
    Env          map[string]string
    BootstrapCmd string             // from runner.InteractiveCommand(prompt, cfg)
}

type AttachOpts struct {
    Runner runner.AgentRunner
}

type Provider interface {
    Create(ctx context.Context, opts CreateOpts) (Sandbox, error)
    Attach(ctx context.Context, ref string, opts AttachOpts) (Sandbox, error)
    IsAlive(ctx context.Context, ref string) bool
}
```

### `internal/sandbox/tmux/provider.go`

Wraps existing tmux logic with no behaviour change. `Create()` calls
`tmux.NewWindowWithDir` + `sendBootstrap`. `Attach()` reconstructs from stored window ID.
`IsAlive()` calls `tmux.WindowExists`.

### DB migration: `internal/db/migrations/028_sandbox_ref.sql`

```sql
ALTER TABLE agents ADD COLUMN sandbox_backend TEXT NOT NULL DEFAULT 'tmux';
ALTER TABLE agents ADD COLUMN sandbox_ref      TEXT;
```

`sandbox_ref` is the backend-agnostic identifier. `tmux_window` stays populated for the tmux
backend for backward compat with existing queries.

### Refactors

- **`internal/agent/agent.go`** — `SpawnWithLayout` accepts `sandbox.Provider`; calls
  `provider.Create()`, stores `sandbox.Ref()` as `sandbox_ref`.
- **`internal/orchestrator/orchestrator.go`** — replace `tmux.WindowExists(...)` with
  `provider.IsAlive(ctx, a.SandboxRef)`.
- **`internal/config/config.go`** — add `SandboxBackend string` (`"tmux"` default).
- **`cmd/maquinista/reconcile_agents.go`**, `spawn_topic_agent.go` — thread provider down.

**Result**: zero behaviour change. All tests pass. tmux is the only live backend.

---

## Phase 2 — Containerise Maquinista + CI

### `Dockerfile` (repo root) — control-plane image

```dockerfile
# Stage 1: Build Next.js dashboard
FROM node:22-alpine AS web-builder
WORKDIR /app/internal/dashboard/web
COPY internal/dashboard/web/package*.json ./
RUN npm ci
COPY internal/dashboard/web/ ./
RUN npm run build

# Stage 2: Pack standalone tarball
FROM web-builder AS tarball
RUN tar -czf /standalone.tgz -C .next/standalone .

# Stage 3: Build Go binary
FROM golang:1.25-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=tarball /standalone.tgz internal/dashboard/standalone.tgz
ARG VERSION=dev
RUN go build -ldflags="-s -w -X main.version=${VERSION}" -o /maquinista ./cmd/maquinista

# Stage 4: Runtime
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    git tmux ca-certificates curl nodejs npm && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
COPY --from=go-builder /maquinista /usr/local/bin/maquinista
COPY scripts/ /usr/local/lib/maquinista/scripts/
ENV PATH="/usr/local/lib/maquinista/scripts:${PATH}"
ENTRYPOINT ["/usr/local/bin/maquinista"]
```

### `Dockerfile.agent` — agent sandbox image (no tmux)

```dockerfile
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    git nodejs npm ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
COPY --from=go-builder /maquinista /usr/local/bin/maquinista
COPY scripts/ /usr/local/lib/maquinista/scripts/
ENV PATH="/usr/local/lib/maquinista/scripts:${PATH}"
ENTRYPOINT ["/usr/local/bin/maquinista", "agent", "run"]
```

This image becomes the squashfs base shared read-only across all kata VMs. Per-agent writes go
into the sparse ext4 overlay layer that Kata creates automatically.

### `.github/workflows/docker.yml`

```yaml
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ubuntu-latest
    permissions: { contents: read, packages: write }
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          push: true
          tags: |
            ghcr.io/brisautomacao/maquinista:${{ github.sha }}
            ghcr.io/brisautomacao/maquinista:latest
      - uses: docker/build-push-action@v6
        with:
          file: Dockerfile.agent
          push: true
          tags: |
            ghcr.io/brisautomacao/maquinista-agent:${{ github.sha }}
            ghcr.io/brisautomacao/maquinista-agent:latest
```

---

## Phase 3 — K8s Cluster Infrastructure

All files land in `brisakube/maquinista/` and are synced via ArgoCD.

### 3.1 KVM-gated: kata-deploy DaemonSet

If Phase 0 confirmed KVM, apply the upstream kata-deploy DaemonSet. It copies kata binaries to
`/opt/kata/` on each node and patches k3s's containerd config
(`/var/lib/rancher/k3s/agent/etc/containerd/config.toml`) to register the runtime handlers:

```toml
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc]
  runtime_type = "io.containerd.kata-fc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc.options]
  ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-fc.toml"
```

The kata-deploy DaemonSet does this automatically — no Terraform changes needed for the
containerd config, no node reprovisioning:

```yaml
# brisakube/maquinista/kata-deploy.yaml
# Pin to a release — do not use :latest in prod
# Full manifest: https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: kata-deploy
  namespace: kube-system
# ... pin upstream manifest here
```

If KVM is absent, skip this file and use gVisor instead (install via the analogous
`gvisor-deploy` or node `postinstall_exec`).

### 3.2 RuntimeClass

```yaml
# brisakube/maquinista/runtimeclass.yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-fc
handler: kata-fc
overhead:
  podFixed: { memory: "60Mi", cpu: "250m" }
scheduling:
  nodeClassification:
    tolerations:
      - { key: kata, operator: Exists, effect: NoSchedule }
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
```

### 3.3 Agent node pool (Terraform — `kube.tf`)

Add to `agent_nodepools` in the brisakube `kube.tf`:

```hcl
{
  name        = "agents"
  server_type = "cx43"      # 8 vCPU / 16 GB — fits ~6–8 concurrent kata VMs
  location    = "nbg1"
  labels      = ["node-role=ai-agents", "kata=true"]
  taints      = ["kata=true:NoSchedule"]
  count       = 1
}
```

And enable autoscaler for scale-to-zero:

```hcl
autoscaler_nodepools = [
  {
    name        = "agents"
    server_type = "cx43"
    location    = "nbg1"
    min_nodes   = 0
    max_nodes   = 3
  }
]
```

Label existing workers so kata pods land only on the `agents` pool:

```bash
kubectl taint node <agents-node> kata=true:NoSchedule
```

---

## Phase 4 — E2B Backend (`internal/sandbox/e2b/`)

Implements `sandbox.Provider` using the E2B managed API. Useful before the self-hosted k8s
path is operational, or as a permanent backend for dev/staging.

### `internal/sandbox/e2b/provider.go`

Uses `github.com/e2b-dev/e2b-go`.

**`Create()`**:
1. `e2b.NewSandbox(ctx, templateID, ...)` — boots Firecracker VM from template
2. `sandbox.Process.Start(bootstrapCmd, envs, workingDir)` — starts agent inside
3. Returns `E2BSandbox` wrapping sandbox + process handles

**`Attach()`**: `e2b.GetSandbox(ctx, sandboxID)` — reconnects to existing VM. No process restart.

**`IsAlive()`**: polls `e2b.GetSandbox` status.

### `E2BSandbox.Driver()` — stdin

```go
type e2bDriver struct{ proc *e2b.Process }
func (d *e2bDriver) Drive(ctx context.Context, text string) error {
    return d.proc.Stdin.Write([]byte(text + "\n"))
}
```

### `E2BSandbox.Tailer()` — transcript

Claude CLI writes JSONL to `~/.claude/projects/<hash>/*.jsonl` inside the sandbox. The tailer:
1. Watches via `sandbox.Filesystem.Watch(path)`
2. Reads new lines via `sandbox.Filesystem.Read(path)`
3. Parses with the existing `TranscriptEvent` JSONL parser (unchanged)

### New config fields

```go
E2BAPIKey     string  // E2B_API_KEY
E2BTemplateID string  // E2B_TEMPLATE_ID (default: "base")
```

### `internal/sandbox/e2b/template.go` + `maquinista template` CLI

```
maquinista template build   -- builds E2B template from Dockerfile.agent, outputs template ID
maquinista template list    -- lists available templates
```

### `go.mod` additions

```
github.com/e2b-dev/e2b-go v1.x
```

---

## Phase 5 — Kata Backend (`internal/sandbox/kata/`)

Implements `sandbox.Provider` by submitting k8s Jobs with `runtimeClassName: kata-fc`.

### `internal/sandbox/kata/provider.go`

```go
type Provider struct {
    client       *kubernetes.Clientset
    namespace    string
    image        string   // ghcr.io/brisautomacao/maquinista-agent:latest
    runtimeClass string   // "kata-fc" | "gvisor" | ""
}

func (p *Provider) Create(ctx context.Context, opts sandbox.CreateOpts) (sandbox.Sandbox, error) {
    job := &batchv1.Job{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "agent-" + opts.AgentID,
            Namespace: p.namespace,
            Labels:    map[string]string{"app": "maquinista-agent", "agent-id": opts.AgentID},
        },
        Spec: batchv1.JobSpec{
            TTLSecondsAfterFinished: int32Ptr(300),
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    RuntimeClassName: &p.runtimeClass,
                    RestartPolicy:    corev1.RestartPolicyNever,
                    Containers: []corev1.Container{{
                        Name:    "agent",
                        Image:   p.image,
                        Command: []string{"sh", "-c", opts.BootstrapCmd},
                        Env:     envMapToK8s(opts.Env),
                        Resources: corev1.ResourceRequirements{
                            Requests: corev1.ResourceList{
                                corev1.ResourceCPU:    resource.MustParse("500m"),
                                corev1.ResourceMemory: resource.MustParse("1Gi"),
                            },
                            Limits: corev1.ResourceList{
                                corev1.ResourceCPU:    resource.MustParse("2"),
                                corev1.ResourceMemory: resource.MustParse("4Gi"),
                            },
                        },
                    }},
                },
            },
        },
    }
    _, err := p.client.BatchV1().Jobs(p.namespace).Create(ctx, job, metav1.CreateOptions{})
    if err != nil {
        return nil, err
    }
    return &KataSandbox{client: p.client, ns: p.namespace, agentID: opts.AgentID}, nil
}

func (p *Provider) IsAlive(ctx context.Context, ref string) bool {
    job, err := p.client.BatchV1().Jobs(p.namespace).Get(ctx, "agent-"+ref, metav1.GetOptions{})
    if err != nil {
        return false
    }
    return job.Status.Active > 0
}
```

### `internal/sandbox/kata/driver.go` — stdin via k8s exec API

Replaces `tmux.SendKeysWithDelay`. Uses `client-go` streaming exec into the running pod.

### `internal/sandbox/kata/tailer.go` — transcript via pod log stream

Agent pod entrypoint tails `~/.claude/projects/<hash>/*.jsonl` and writes events to stdout.
Tailer streams pod logs via `client-go` `GetLogs(Follow: true)` and parses with the existing
`TranscriptEvent` JSONL parser. No per-agent PVC needed.

### Orchestrator: Job watch loop

```go
// internal/orchestrator/orchestrator.go — added alongside the poll loop
watcher, _ := client.BatchV1().Jobs(ns).Watch(ctx, metav1.ListOptions{
    LabelSelector: "app=maquinista-agent",
})
for event := range watcher.ResultChan() {
    job := event.Object.(*batchv1.Job)
    if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
        db.Exec("UPDATE agents SET status='dead' WHERE id=$1", agentIDFromJob(job))
        db.Exec("SELECT pg_notify('agent_events', $1)", agentIDFromJob(job))
    }
}
```

### New config fields

```go
SandboxBackend   string  // "tmux" | "e2b" | "kata"
K8sNamespace     string  // MAQUINISTA_NAMESPACE, default "maquinista"
K8sInCluster     bool    // auto-detected from KUBERNETES_SERVICE_HOST
K8sRuntimeClass  string  // "kata-fc" | "gvisor" | ""
AgentImage       string  // MAQUINISTA_AGENT_IMAGE
```

---

## Phase 6 — K8s Manifests (brisakube repo)

### Namespace + RBAC (`brisakube/maquinista/namespace.yaml`)

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: maquinista
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: maquinista-orchestrator
  namespace: maquinista
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-job-manager
  namespace: maquinista
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create", "get", "list", "watch", "delete"]
  - apiGroups: [""]
    resources: ["pods", "pods/log"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: agent-job-manager
  namespace: maquinista
subjects:
  - { kind: ServiceAccount, name: maquinista-orchestrator }
roleRef:
  { kind: Role, name: agent-job-manager, apiGroup: rbac.authorization.k8s.io }
```

### PostgreSQL (`brisakube/maquinista/postgres.yaml`)

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: maquinista
spec:
  selector:
    matchLabels: { app: postgres }
  serviceName: postgres
  replicas: 1
  template:
    metadata:
      labels: { app: postgres }
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - { name: POSTGRES_DB, value: maquinistadb }
            - { name: POSTGRES_USER, value: maquinista }
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef: { name: maquinista-env, key: POSTGRES_PASSWORD }
          ports:
            - containerPort: 5432
          volumeMounts:
            - { name: pgdata, mountPath: /var/lib/postgresql/data }
          livenessProbe:
            exec:
              command: ["pg_isready", "-U", "maquinista"]
            initialDelaySeconds: 15
            periodSeconds: 10
  volumeClaimTemplates:
    - metadata:
        name: pgdata
      spec:
        accessModes: ["ReadWriteOnce"]
        storageClassName: hcloud-volumes
        resources:
          requests:
            storage: 10Gi
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: maquinista
spec:
  selector: { app: postgres }
  ports:
    - { port: 5432, targetPort: 5432 }
  clusterIP: None
```

### Orchestrator (`brisakube/maquinista/orchestrator.yaml`)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: maquinista-orchestrator
  namespace: maquinista
spec:
  replicas: 1
  selector:
    matchLabels: { app: maquinista-orchestrator }
  template:
    metadata:
      labels: { app: maquinista-orchestrator }
    spec:
      serviceAccountName: maquinista-orchestrator
      containers:
        - name: orchestrator
          image: ghcr.io/brisautomacao/maquinista:latest
          command: ["maquinista", "orchestrator", "start", "--foreground"]
          envFrom:
            - secretRef: { name: maquinista-env }
          env:
            - { name: SANDBOX_BACKEND, value: kata }
            - { name: MAQUINISTA_NAMESPACE, value: maquinista }
          resources:
            requests: { cpu: "100m", memory: "128Mi" }
            limits:   { cpu: "500m", memory: "512Mi" }
```

### Dashboard (`brisakube/maquinista/dashboard.yaml`)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: maquinista-dashboard
  namespace: maquinista
spec:
  replicas: 1
  selector:
    matchLabels: { app: maquinista-dashboard }
  template:
    metadata:
      labels: { app: maquinista-dashboard }
    spec:
      containers:
        - name: dashboard
          image: ghcr.io/brisautomacao/maquinista:latest
          command: ["maquinista", "dashboard", "start", "--foreground"]
          envFrom:
            - secretRef: { name: maquinista-env }
          ports:
            - containerPort: 3000
          resources:
            requests: { cpu: "200m", memory: "256Mi" }
            limits:   { cpu: "1", memory: "1Gi" }
---
apiVersion: v1
kind: Service
metadata:
  name: maquinista-dashboard
  namespace: maquinista
spec:
  selector: { app: maquinista-dashboard }
  ports:
    - { port: 3000, targetPort: 3000 }
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: maquinista-dashboard
  namespace: maquinista
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  ingressClassName: nginx
  tls:
    - { hosts: [maquinista.brisa.ai], secretName: maquinista-tls }
  rules:
    - host: maquinista.brisa.ai
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: maquinista-dashboard
                port: { number: 3000 }
```

### ArgoCD Application (`brisakube/argocd/apps/maquinista.yaml`)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: maquinista
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/brisautomacao/brisakube
    targetRevision: main
    path: maquinista
  destination:
    server: https://kubernetes.default.svc
    namespace: maquinista
  syncPolicy:
    automated: { prune: true, selfHeal: true }
    syncOptions: [CreateNamespace=true]
```

---

## Files Changed / Created

### maquinista repo

| File | Change |
|---|---|
| `internal/sandbox/sandbox.go` | **NEW** — `Sandbox`, `Provider`, `CreateOpts`, `AttachOpts` interfaces |
| `internal/sandbox/tmux/provider.go` | **NEW** — wraps existing tmux logic |
| `internal/sandbox/e2b/provider.go` | **NEW** — E2B SDK integration |
| `internal/sandbox/e2b/template.go` | **NEW** — template build/list helpers |
| `internal/sandbox/kata/provider.go` | **NEW** — k8s Job backend |
| `internal/sandbox/kata/driver.go` | **NEW** — stdin via k8s exec API |
| `internal/sandbox/kata/tailer.go` | **NEW** — stdout via pod log streaming |
| `internal/db/migrations/028_sandbox_ref.sql` | **NEW** — `sandbox_backend`, `sandbox_ref` columns |
| `internal/agent/agent.go` | **MODIFY** — `SpawnWithLayout` accepts `sandbox.Provider` |
| `internal/orchestrator/orchestrator.go` | **MODIFY** — dead-check via `provider.IsAlive` + k8s Job watch |
| `internal/config/config.go` | **MODIFY** — add backend + k8s fields |
| `cmd/maquinista/reconcile_agents.go` | **MODIFY** — thread provider down |
| `cmd/maquinista/spawn_topic_agent.go` | **MODIFY** — thread provider down |
| `cmd/maquinista/cmd_template.go` | **NEW** — `maquinista template` subcommand |
| `Dockerfile` | **NEW** — multi-stage control-plane image |
| `Dockerfile.agent` | **NEW** — agent-only image (no tmux) |
| `.github/workflows/docker.yml` | **NEW** — build + push to GHCR |
| `go.mod` / `go.sum` | **MODIFY** — add `e2b-go`, `k8s.io/client-go` |

### brisakube repo

| File | Change |
|---|---|
| `maquinista/namespace.yaml` | **NEW** — Namespace, ServiceAccount, Role, RoleBinding |
| `maquinista/kvm-check.yaml` | **NEW** — pre-flight Job |
| `maquinista/kata-deploy.yaml` | **NEW** — kata-containers DaemonSet (if KVM present) |
| `maquinista/runtimeclass.yaml` | **NEW** — `kata-fc` + `gvisor` RuntimeClass |
| `maquinista/postgres.yaml` | **NEW** — StatefulSet + headless Service |
| `maquinista/orchestrator.yaml` | **NEW** — Deployment |
| `maquinista/dashboard.yaml` | **NEW** — Deployment + Service + Ingress |
| `argocd/apps/maquinista.yaml` | **NEW** — ArgoCD Application |

### kube.tf (brisakube Terraform config)

| Config | Change |
|---|---|
| `agent_nodepools` | **MODIFY** — add `agents` pool (cx43, `kata=true:NoSchedule` taint) |
| `autoscaler_nodepools` | **MODIFY** — add `agents` pool (0–3 nodes) |

---

## Rollout Order

| Phase | Repo | Depends on |
|-------|------|-----------|
| 0 — KVM check | brisakube | — |
| 1 — Sandbox abstraction (tmux wraps) | maquinista | — |
| 2 — Dockerfile + CI | maquinista | — |
| 3 — K8s infra (kata-deploy, RuntimeClass, node pool) | brisakube + kube.tf | Phase 0 |
| 4 — E2B backend | maquinista | Phase 1 |
| 5 — kata backend | maquinista | Phase 1 + Phase 3 confirmed working |
| 6 — K8s manifests + ArgoCD | brisakube | Phase 2 image pushed |

Phases 1, 2, and 0 can all start in parallel. Phase 4 (E2B) can be used as an integration
test for the sandbox abstraction before the self-hosted kata path is ready.

---

## Verification

1. **Phase 1 regression**: `go test ./...` passes; `SANDBOX_BACKEND=tmux maquinista start` behaves identically.
2. **Phase 2**: `docker build -f Dockerfile .` and `docker build -f Dockerfile.agent .` both succeed.
3. **Phase 3**: `kubectl get runtimeclass` shows `kata-fc`; test pod with `runtimeClassName: kata-fc` boots.
4. **Phase 4 (E2B)**: `E2B_API_KEY=... SANDBOX_BACKEND=e2b maquinista start` → spawn agent → Telegram message arrives → confirm response.
5. **Phase 5 (kata)**: `SANDBOX_BACKEND=kata maquinista orchestrator start --foreground` → task created → `kubectl get jobs -n maquinista` shows `agent-<id>` Active → job completes → transcript in DB.
6. **Phase 6**: `kubectl get pods -n maquinista` shows `postgres`, `orchestrator`, `dashboard` Running; dashboard reachable at `maquinista.brisa.ai`.
7. **End-to-end**: Telegram message → task in Postgres → orchestrator spawns kata-fc Job → agent runs in Firecracker VM → response delivered → Job TTL-deleted.

---

## Interaction with Other Plans

- **`active/retire-legacy-tmux-paths.md`** — Phase 5 (kata driver) supersedes
  `tmux.SendKeysWithDelay` for all agent-bound messages. The two plans converge there.
- **`active/per-agent-sidecar.md`** — the sidecar's `TranscriptTailer` interface is reused by
  all three backends; only the file-reading transport changes.
- **`active/productization-saas.md`** — depends on this plan: multi-tenant SaaS requires
  per-agent VM isolation which the kata backend provides.
