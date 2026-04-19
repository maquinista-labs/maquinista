# Plan: E2B Sandbox Integration for Maquinista

## Context

Maquinista currently runs agents as tmux windows on the host machine — no kernel-level isolation, no resource limits. The goal is to integrate E2B (Firecracker microVM sandboxes) as the execution backend so each agent runs in a fully isolated VM, while keeping the door open for self-hosted Kubernetes with Kata Containers later.

The approach is two-phase:
1. **Phase 1** — introduce a `Sandbox` abstraction layer with zero behavior change (tmux stays the default)
2. **Phase 2** — implement the E2B backend behind that interface

---

## Architecture Overview

```
Telegram → agent_inbox → PtyDriver → [sandbox stdin] → claude CLI → [sandbox stdout/fs] → TranscriptTailer → agent_outbox → Telegram
```

The sidecar already has two clean interfaces (`PtyDriver`, `TranscriptTailer`) that represent the I/O boundary. The new `Sandbox` interface wraps both, adds lifecycle methods, and is implemented by both tmux and E2B backends.

---

## Phase 1 — Sandbox Abstraction (no behavior change)

### New package: `internal/sandbox/sandbox.go`

```go
type Sandbox interface {
    Ref() string                               // stored in DB; tmux window ID or E2B sandbox ID
    Driver() sidecar.PtyDriver                 // stdin feed into the agent
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
    Runner runner.AgentRunner       // needed to reconstruct tailer profile
}

type Provider interface {
    Create(ctx context.Context, opts CreateOpts) (Sandbox, error)
    // Attach reconnects to a sandbox that survived a daemon restart.
    Attach(ctx context.Context, ref string, opts AttachOpts) (Sandbox, error)
    IsAlive(ctx context.Context, ref string) bool
}
```

### New file: `internal/sandbox/tmux/provider.go`

Wraps existing tmux logic. `Create()` calls `tmux.NewWindowWithDir` + `sendBootstrap` (extracted from `agent.go`). `Attach()` reconstructs from a stored window ID. `IsAlive()` calls `tmux.WindowExists`.

Driver implementation: wraps `tmux.SendKeys` + `tmux.SendEnter`.  
Tailer implementation: wraps existing file-based JSONL tailer (unchanged).

### DB migration: `internal/db/migrations/012_sandbox_ref.sql`

```sql
ALTER TABLE agents ADD COLUMN sandbox_backend TEXT NOT NULL DEFAULT 'tmux';
ALTER TABLE agents ADD COLUMN sandbox_ref      TEXT;
```

`sandbox_ref` replaces the role of `tmux_window` as the backend-agnostic identifier. `tmux_window` stays populated for the tmux backend (backward compat with existing queries).

### Refactors

**`internal/agent/agent.go`** — `SpawnWithLayout` currently calls `tmux.NewWindowWithDir` and `sendBootstrap` directly. Refactor to accept a `sandbox.Provider` parameter; call `provider.Create(ctx, opts)` and store `sandbox.Ref()` as `sandbox_ref` in DB alongside `tmux_window` for tmux backend.

**Orchestrator dead-check** (`internal/orchestrator/orchestrator.go`) — replace `tmux.WindowExists(session, a.TmuxWindow)` with `provider.IsAlive(ctx, a.SandboxRef)`.

**Sidecar construction** (wherever `SidecarRunner` is wired up) — construct `driver` and `tailer` from `sandbox.Driver()` and `sandbox.Tailer()` instead of building them from tmux primitives.

**`cmd/maquinista/reconcile_agents.go`** and `spawn_topic_agent.go` — pass provider down instead of constructing tmux directly.

**`internal/config/config.go`** — add:
```go
SandboxBackend string  // "tmux" (default) | "e2b" | "kata" (future)
```

### Result of Phase 1

Zero behavior change. All tests pass. Tmux is the only backend. Code is ready for Phase 2.

---

## Phase 2 — E2B Backend

### New file: `internal/sandbox/e2b/provider.go`

Uses the E2B Go SDK (`github.com/e2b-dev/e2b-go`).

**`Create()`:**
1. Call `e2b.NewSandbox(ctx, templateID, ...)` — boots Firecracker VM from template
2. Start the agent process inside: `sandbox.Process.Start(bootstrapCmd, envs, workingDir)`
3. Return `E2BSandbox` wrapping the sandbox + process handles

**`Attach()`:**
1. Call `e2b.GetSandbox(ctx, sandboxID)` — reconnect to existing VM
2. No process restart — the claude CLI process is still running inside

**`IsAlive()`:** Poll `e2b.GetSandbox` or check sandbox status.

### `E2BSandbox.Driver()` — stdin to E2B process

```go
type e2bDriver struct{ proc *e2b.Process }

func (d *e2bDriver) Drive(ctx context.Context, text string) error {
    return d.proc.Stdin.Write([]byte(text + "\n"))
}
```

### `E2BSandbox.Tailer()` — transcript from E2B filesystem

Claude CLI writes JSONL transcripts to `~/.claude/projects/<hash>/*.jsonl` inside the sandbox. The tailer:
1. Watches the transcript directory via `sandbox.Filesystem.Watch(path)`
2. On new lines, reads via `sandbox.Filesystem.Read(path)`
3. Parses each line as `TranscriptEvent` (same JSONL parser already used in current tailer)

This reuses the existing JSONL parsing logic — only the file reading mechanism changes.

### New config fields

```go
E2BAPIKey    string  // E2B_API_KEY env var
E2BTemplateID string // E2B_TEMPLATE_ID env var (default: "base")
```

### New file: `internal/sandbox/e2b/template.go`

Helper to build/list E2B templates. Called by a new CLI command.

### New CLI command: `maquinista template`

```
maquinista template build   -- builds E2B template from Dockerfile, outputs template ID
maquinista template list    -- lists available templates
```

**`cmd/maquinista/cmd_template.go`** — cobra subcommand delegating to `e2b/template.go`.

### `Dockerfile.e2b` (new file in repo root)

The base image baked into the E2B template:
- Base: Ubuntu 22.04
- Installs: claude CLI, maquinista scripts (bind-mounted from binary), git, common tools
- Pre-clones target repos (or left empty if repos are cloned at runtime via git)
- Each sandbox gets a COW copy automatically (Firecracker + OverlayFS under the hood via E2B)

### `go.mod` additions

```
github.com/e2b-dev/e2b-go v1.x
```

---

## Kubernetes Future Path

No additional changes needed after Phase 1. Add `internal/sandbox/kata/provider.go`:
- `Create()` → submit a K8s Job with `runtimeClassName: kata-fc`
- `Attach()` → re-connect to existing Job pod
- `IsAlive()` → check pod status via K8s API

Config: `SANDBOX_BACKEND=kata` + standard kubeconfig.

---

## Files Changed / Created

| File | Change |
|---|---|
| `internal/sandbox/sandbox.go` | **NEW** — `Sandbox`, `Provider`, `CreateOpts`, `AttachOpts` interfaces |
| `internal/sandbox/tmux/provider.go` | **NEW** — wraps existing tmux logic |
| `internal/sandbox/e2b/provider.go` | **NEW** — E2B SDK integration |
| `internal/sandbox/e2b/template.go` | **NEW** — template build/list helpers |
| `internal/db/migrations/012_sandbox_ref.sql` | **NEW** — `sandbox_backend`, `sandbox_ref` columns |
| `internal/agent/agent.go` | **MODIFY** — `SpawnWithLayout` accepts `sandbox.Provider` |
| `internal/orchestrator/orchestrator.go` | **MODIFY** — dead-check via `provider.IsAlive` |
| `internal/config/config.go` | **MODIFY** — add `SandboxBackend`, `E2BAPIKey`, `E2BTemplateID` |
| `cmd/maquinista/reconcile_agents.go` | **MODIFY** — thread provider down |
| `cmd/maquinista/spawn_topic_agent.go` | **MODIFY** — thread provider down |
| `cmd/maquinista/cmd_template.go` | **NEW** — `maquinista template` subcommand |
| `Dockerfile.e2b` | **NEW** — E2B sandbox template image |
| `go.mod` / `go.sum` | **MODIFY** — add `e2b-go` dependency |

---

## Verification

1. **Phase 1 regression**: `go test ./...` passes; `maquinista start` behaves identically with `SANDBOX_BACKEND=tmux`
2. **E2B smoke test**: `E2B_API_KEY=... SANDBOX_BACKEND=e2b maquinista start` → spawn one agent → send a Telegram message → confirm response arrives (agent ran inside E2B VM)
3. **Dead agent recovery**: kill E2B sandbox manually → next orchestrator tick detects it dead → respawns
4. **Restart recovery**: restart daemon with `SANDBOX_BACKEND=e2b` → `Attach()` reconnects to live sandbox → inbox messages resume without losing agent state
5. **Template build**: `maquinista template build` succeeds → returned template ID used for next sandbox create
