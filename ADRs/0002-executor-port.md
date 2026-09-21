# ADR-0002: Executor Port — Decouple Agent Session I/O from tmux (Ports & Adapters)

- **Status:** Proposto (pending Otavio's ok)
- **Date:** 2026-09-21
- **Deciders:** Otavio
- **Scope:** `internal/tmux` and every call site that drives agent panes (sidecar, spawn, reconcile, bot, planner, monitor); introduces `internal/executor`
- **Depends on / feeds:** prerequisite for ADR-0003 (Substrate executor); adjacent to ADR-0001 Option E (job-style runners need the same seam)

## Context

tmux is maquinista's execution substrate, but it holds **zero durable state** —
Postgres is the system of record (plans §0) and tmux is deliberately a
stateless PTY holder. That makes the tmux layer a textbook hexagonal port: one
package behind which the substrate is swappable.

Current surface, `internal/tmux/tmux.go` — **25 exported functions, ~90 call
sites** across 11 files:

| Vocabulary group | Functions | Callers |
|---|---|---|
| Session lifecycle | `SessionExists`, `EnsureSession`, `KillSession`, `InsideTmux` | sidecar, spawn, bot, reconcile |
| Window lifecycle | `NewWindow`, `NewWindowWithDir`, `WindowExists`, `GetWindowID`, `RenameWindow`, `KillWindow`, `ListWindows`, `CleanupInitWindow` | reconcile_agents, spawn_topic_agent, cmd_planner, bot ×3, agentspawn, agent |
| I/O (drive the TUI) | `SendKeys`, `SendKeysWithDelay`, `SendEnter`, `SendSpecialKey` | sidecar drive loop, bot (long-prompt temp-file path), planner |
| I/O (observe) | `CapturePane`, `CapturePaneLines`, `WaitForReady`, `DisplayMessage` | monitor profiles, spawn readiness, bot status |
| Human UX (tmux-only) | `AttachOrSwitch`, `AttachSession`, `SwitchWindow` | bot commands, CLI |

Two structural facts anchor the design:

1. **The I/O is TUI-keyboard-shaped** (`send-keys`/`capture-pane`), not
   exec-shaped. Whatever replaces tmux must expose a PTY-like read/write stream
   or the `MonitorProfile` layer (TUI scraping) collapses.
2. **Multi-tenancy is coming** (ADR-0003: multiple users on a shared substrate).
   The port must carry tenant context from day one, even though the tmux
   implementation ignores it — retrofitting tenant into a single-user-shaped
   port later is exactly the churn this ADR exists to avoid.

Drivers: Google's Agent Substrate/AX (open-sourced 2026-09-20) as a future
second implementation; hermetic tests (today nothing fakes tmux); and the
ADR-0001 Option E class of non-interactive runners that don't fit panes.

## Options Considered

| | A. Session-centric port, fat tmux adapter | B. Literal port (tmux-shaped interface) | C. Do nothing, port straight to Substrate later |
|---|---|---|---|
| Port vocabulary | Create/Write/Read/Alive/Kill/List per *agent session* | mirrors current 25 funcs 1:1 | none — call sites adopt Substrate SDK directly |
| Call-site churn | mechanical, one-time (~90 sites, ~11 files) | smallest | largest, and premature |
| Substrate adapter fit | natural (Task CRD ↔ session) | fights it: windows/panes/`send-keys` names leak | n/a |
| Test fakes | trivial | trivial | none possible without a cluster |
| Risk | over-abstraction if we invent methods no second impl needs | Substrate adapter contorts; port rewritten anyway | 90 sites couple deeper to tmux daily; tenant context lands late |

## Decision

**Option A — introduce `internal/executor` with a session-centric port; tmux
becomes the first adapter; the tmux-only human-UX surface moves to an optional
extension interface.**

Sketch (exact shapes settle in EX-01; do not gold-plate):

```go
type Spec struct { // one agent session
    TenantID string            // NEW — threaded from day one; tmux adapter ignores it
    Name     string            // stable id: agentID / topic / window name
    Dir      string            // worktree cwd
    Command  string            // full runner command line
    Env      map[string]string // replaces tmux set-environment dance
}

type Executor interface {
    Create(ctx context.Context, Spec) (Handle, error)        // EnsureSession+NewWindow
    Write(ctx context.Context, Handle, keys string) error    // SendKeys family (literal)
    WriteKey(ctx context.Context, Handle, key string) error  // Enter / special keys
    Read(ctx context.Context, Handle, ReadOpts) (string, error) // CapturePane/CapturePaneLines
    Alive(ctx context.Context, Handle) (bool, error)         // WindowExists + IsWindowDead → ErrDead
    Kill(ctx context.Context, Handle) error                  // KillWindow
    List(ctx context.Context) ([]HandleInfo, error)          // ListWindows → reconcile_agents
}

// Optional; only tmux implements today. Call sites feature-detect:
type HumanUI interface { Attach(h Handle) error; Switch(h Handle) error; Rename(h Handle, name string) error }
```

- `IsWindowDead` becomes a sentinel (`executor.ErrDead`) — error *classification*
  stays a package-level helper, not a port method.
- `WaitForReady`/`DisplayMessage` are call-site polling loops over
  `Read`/`Alive`, not port methods (verified: both compose from existing prims).
- **YAGNI guard:** the port grows only when a *second real implementation*
  (test fake first, Substrate second) demands it. No speculative methods.

### Task checklist

| Task | Files | Size | Est |
|---|---|---|---|
| EX-00 Parity test: pin current tmux behavior (spawn→keys→capture→kill) as the contract the adapter must pass | `internal/executor/tmux_test.go` | ~150 ln | 2-3 h |
| EX-01 Port + tmux adapter wrapping existing funcs unchanged; `ErrDead` sentinel | `internal/executor/`, thin `internal/tmux` | ~200 ln | 1 d |
| EX-02 Call-site migration, package by package (sidecar, agentspawn, agent, bot ×3, cmd_planner, spawn_topic_agent, reconcile_agents, monitor); behavior-identical | 11 files | ~90 edits, mechanical | 0.5-1 d |
| EX-03 Thread `Spec.TenantID` from owner binding at spawn (tmux impl ignores; used by ADR-0003 Phase 2) | spawn paths | ~20 ln | 1-2 h |
| EX-04 In-memory fake + rerun affected `go test` packages hermetically | `internal/executor/fake.go` | ~80 ln | 2-3 h |

**Total: ≈ 2-3 dev days.** No schema changes, no `arch/` restructuring beyond a
new `arch/executor.md` note (CLAUDE.md rule).

## Consequences

- **Positive:** Substrate (ADR-0003) becomes an additive adapter, not a rewrite;
  agent tests run hermetically; the seam ADR-0001 Option E needs for job-style
  runners already exists; `TenantID` present before the first multi-tenant impl.
- **Negative:** one indirection at ~90 call sites; risk of vocabulary drift if
  EX-01 invents methods no second impl uses (mitigated by the YAGNI guard and
  by writing the fake *before* Substrate ever influences the port).
- **Neutral:** `tmux attach` human UX keeps working unchanged (HumanUI); the
  `TMUX_SESSION_NAME` config stays meaningful only for the tmux adapter.

## References

- `internal/tmux/tmux.go` — the 25-func surface this port wraps (this session, post-a3c5cc1)
- `cmd/maquinista/reconcile_agents.go:190` — the List() consumer that snapshot-resume must replace (ADR-0003)
- ADR-0003 — Substrate/Hetzner integration on top of this port
- ADR-0001 Option E — job-style runner class that needs an exec seam, not a pane
- Playa substrate-feasibility check 2026-09-21: no `/dev/kvm`, VT-x absent → local microVMs impossible; the second adapter must live on remote hardware
