# Architecture

Durable truths about how maquinista works. Each file covers one concern.
These are not plans (see `plans/`) and not user docs (see `docs/`) — they
describe invariants that cut across the codebase and should stay accurate
as the system evolves.

Files may contain `## TODO` sections that reference active plans. Those
sections are stubs waiting to be filled in once the plan lands.

| File | What it covers |
|------|----------------|
| [messaging.md](messaging.md) | Shared inbox/outbox model, relay fanout, Telegram vs dashboard delivery |
| [routing.md](routing.md) | Four-tier message routing ladder, topic bindings, A2A routing |
| [agent-lifecycle.md](agent-lifecycle.md) | Status model, spawn paths, reconcile loop, session resume |
| [runners.md](runners.md) | Claude / OpenCode / OpenClaude, launch command, transcript tailing |
| [soul-and-identity.md](soul-and-identity.md) | Soul schema, rendering, memory blocks, identity across restarts |
| [workspaces.md](workspaces.md) | Shared / agent / task scopes, git worktrees, workspace switching |
| [orchestration.md](orchestration.md) | Three-agent trio, task graph, orchestrator engine, job registry |
| [sidecar.md](sidecar.md) | Per-agent supervisor design, current transitional state, migration path |
| [dashboard.md](dashboard.md) | Next.js + Go architecture, embedding, API routes, tunnel, auth |
| [database.md](database.md) | Postgres as single source of truth, migrations, NOTIFY channels, key tables |
