# ADR-0003: Substrate/AX Executor on a Dedicated Hetzner Box (Multi-Tenant)

- **Status:** Proposto (pending Otavio's ok)
- **Date:** 2026-09-21
- **Deciders:** Otavio
- **Scope:** execution infrastructure (new box + network link), `internal/executor` second adapter, multi-tenant isolation model
- **Depends on:** ADR-0002 (the `Executor` port this integrates through; `Spec.TenantID` already threaded)

## Context

Google open-sourced its agentic infra on 2026-09-20 (rakyll announcement). Both
repos **verified by direct inspection 2026-09-21**:

- **Agent Substrate** — `github.com/agent-substrate/substrate` (own org, not google/):
  a system **built on top of Kubernetes** — actors mapped onto worker *Pods*;
  control plane out of the hot path; deploy manifests in-repo, dev tooling
  GCP-biased (`tools/setup-gcp`). **Workload isolation today is gVisor
  (`runsc`) on pods** (their AGENTS.md security section); microVM via
  cloud-hypervisor exists in code (`cmd/ateom-microvm`) but "runtime modularity
  (gVisor, microVMs, etc)" is **roadmap, not current**. **x86_64-centric**:
  zero `aarch64/arm64` mentions in code or docs; KVM-clock code comments assume x86.
- **AX** — `github.com/google/ax`: kubectl-shaped orchestrator on top, 4 CRDs
  (Task/Workspace/Gateway/Model, `ax.io/v1alpha1`), `ax apply/watch/ssh`,
  `ax suspend`/`resume`; in-cluster server + controller + redis (`deploy/`).

**Two consequences that soften/redirect the hardware constraint:**

1. **gVisor does not need `/dev/kvm`** (systrap/ptrace platforms). Since gVisor
   is Substrate's *current* isolation, the verified "no nested virt" blocker on
   playa/Hetzner-Cloud only applies to the **future microVM mode**. A plain x86
   Linux box — even a cloud VP — can host a single-node k3s + Substrate + AX
   stack today. `/dev/kvm` becomes a hard requirement again only when
   microVM modularity lands.
2. **Apple Silicon is ruled out as worker today**: macOS unsupported (Linux +
   K8s + gVisor stack), and ARM64 Linux is unproven upstream (zero ARM
   mentions). A Mac would need an x86_64 Linux VM under emulation — not
   serious. Revisit only if upstream lands ARM64.

Why this is attractive to maquinista specifically:

- **Isolation**: agents currently run as the same UID on playa with full host
  access, reading Telegram messages from third parties — one prompt injection
  from disaster. Substrate gives a real boundary plus **per-tenant egress
  allowlists** (Gateway), which is the capability tmux structurally cannot give.
- **Multi-tenancy**: maquinista is gaining multiple users; tenants must not
  share credentials, repos, or network reach.
- **Restart-safety**: `reconcile_agents.go` reattaches *live* panes and dies
  with the host; snapshot-resume is strictly stronger.

Hard constraint, verified on playa 2026-09-21: **no `/dev/kvm`, no VT-x flags,
2 vCPU / 3.7 GB RAM**. Substrate's microVM mode cannot run on the orchestrator
host. The same applies to **Hetzner's Cloud product line** (the regular VPs spun
from the console): Hetzner's official FAQ states flatly that nested
virtualization "is not possible on cloud server" (docs.hetzner.com/cloud/servers/faq,
fetched 2026-09-21). What *does* work is Hetzner's other line, **Robot —
dedicated bare-metal servers**: the worker rents the whole physical machine, so
`/dev/kvm` is real hardware access and Firecracker-class microVMs run natively.
Hence: orchestrator stays on playa, execution moves to a new Robot box.

## Options Considered

| | A. Substrate/AX on dedicated Hetzner Robot | B. e2b (hosted sandboxes) | C. Plain K3s + gVisor/Kata, no Substrate | D. Defer (tmux-only) |
|---|---|---|---|---|
| Ops burden | one box + control plane; day-1 software | zero (hosted SDK) | mature tooling, we still build actor mgmt | zero |
| Per-tenant egress policy | Gateway CRD, first-class | limited/not exposed at that level | DIY NetworkPolicy | impossible |
| Long-lived stateful actors + snapshot resume | the pitch | ephemeral-by-default (their edge case) | weak (no RAM snapshots) | n/a |
| Density on cheap silicon | 30× oversubscription | pay-per-second | low | n/a |
| Cost | ≈ €50-60/mo box | metered, multiplies with tenants | box, smaller | €0 |
| Day-1 churn risk (the ADR-0001-correction lesson) | **high — gated by S-00** | low | low | none |

## Decision

**Option A, phased behind a go/no-go spike.** tmux stays the default executor
throughout; Substrate arrives as an *additional* adapter, never a flag day.

### Phase 0 — Spike (go/no-go, ~1 day)

Provision the smallest Robot AX-class box (8 c / 64 GB ≈ €50-60/mo — plenty for
≤20 concurrent sandboxes), install Substrate, run a hello-world Task, then
**falsify the two load-bearing assumptions**:

- **S-00a PTY fidelity**: is the console stream TUI-grade? `MonitorProfile`
  scraping dies if it's exec-only. (`ax ssh` existing suggests yes — verify.)
- **S-00b Transcript egress**: every `TranscriptSource` tails JSONL on local
  disk (`~/.claude/projects/...`). How do transcripts leave the sandbox — host
  mounts, export API, or do we ship a push shim? **This is the single biggest
  unknown; if ugly, the port cost per runner multiplies.**

No-go on either → this ADR parks as D-with-triggers (see Revisit).

### Phase 1 — Adapter (3-5 days)

`internal/executor/substrate.go` implementing the ADR-0002 port: Create→Task,
Write/WriteKey→PTY stream, Read→stream backfill, List→Task listing,
Kill→delete; reconcile path becomes snapshot-resume. Single tenant (us),
per-agent `executor=substrate` flag, shadow mode — tmux default, opt-in agents.

### Phase 2 — Multi-tenancy (3-5 days)

Tenant model, in order of what already exists:

- **Tenant id**: the routing tier-2 *owner binding* already identifies a user —
  it becomes the tenant key flowing through `Spec.TenantID` (EX-03). A
  dedicated `tenants` table is added only when tenant ≠ owner becomes real.
- **Hard isolation invariants** (each maps to an AX primitive):
  - one AX namespace per tenant — Task/Workspace never cross namespaces;
  - one **Model CRD per tenant** — LLM creds are never shared and never baked
    into images, injected at Task-create from per-tenant secret refs;
  - one **Gateway per tenant, default-deny** — tenant A's injected agent cannot
    reach tenant B's anything, nor arbitrary hosts;
  - transcripts egress to a per-tenant prefix; quota enforcement (concurrent
    sandboxes, wall-clock) in the scheduler; cost attribution via per-Model
    token metering into a `tenant_cost` ledger.
- **Runner auth-porting**: `~/.claude` creds, gh auth, soul injection move from
  "files on playa" to per-tenant secret refs + Workspace pre-wiring. Budget one
  hard day per runner family — this, not the API calls, is the real cost.

### Phase 3 — Ops hardening

Base-image pipeline (node + claude + gh, tenant-agnostic), snapshot retention,
WireGuard playa↔box link with mutual auth, dashboard shows executor + tenant
per agent, `arch/` gains `arch/executor.md` + `arch/tenancy.md`.

## Revisit Triggers (if Phase 0 no-gos, or before provisioning)

- Substrate API shows day-2 stability (changelog cadence, deprecation policy);
- S-00a/S-00b resolved upstream (documented PTY stream + mount/export story);
- multi-tenant demand or RAM pressure on playa materializes anyway.

## Consequences

- **Positive:** real isolation + per-tenant egress for the first time; density
  and sub-500 ms resume on cheap dedicated silicon; agents decoupled from the
  bot host; host-reboot-proof session recovery.
- **Negative:** second always-on box (≈ €50-60/mo); code written against day-1
  APIs (mitigated: adapter isolation + Phase 0 gate + revisit triggers); per-
  runner auth re-imaging is fiddly and only partially automatable.
- **Neutral:** Postgres remains system of record on playa; network hop adds
  single-digit-ms latency to the drive loop — irrelevant at human-interactive
  cadence; e2b (Option B) stays the fallback if self-hosting ops bites.

## References

- ADR-0002 — the `Executor` port (this integration's only maquinista-side seam)
- Playa check 2026-09-21: `/dev/kvm` absent, `grep -cE 'vmx|svm' /proc/cpuinfo` = 0, 2 vCPU / 3.7 GB
- Hetzner Cloud FAQ — "Can I run virtual machines on cloud servers?" → "No, this is not possible on cloud server." (docs.hetzner.com/cloud/servers/faq, fetched 2026-09-21)
- rakyll announcement 2026-09-20; repos verified by inspection 2026-09-21:
  github.com/agent-substrate/substrate + github.com/google/ax (shallow clones, /tmp)
- ADR-0001 "Correction" pattern — why Phase 0 gates day-1-software commitments
- ADR-0001 Option E — job-style runners that would land on Substrate `headless`-style Tasks rather than panes
