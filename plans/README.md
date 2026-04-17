# Plans

> All plans adhere to §0 of `reference/maquinista-v2.md`: **Postgres is the system of record.**

## Layout

- **`active/`** — work not yet done or only partially shipped. Pick from here when starting a new implementation track.
- **`reference/`** — authoritative design docs (living). Read these to understand *why* the system is shaped the way it is.
- **`archive/`** — plans whose implementation work is complete, or which have been superseded. Kept for historical context; do not start new work against them.

## Index

### Active (pending work)

| Plan | Status | Notes |
|---|---|---|
| [`active/agent-soul-db-state.md`](active/agent-soul-db-state.md) | shipped (Phases 1–4) | DB-backed persona / identity + soul CLI + injection scanner. |
| [`active/agent-memory-db.md`](active/agent-memory-db.md) | shipped (Phases 0–5, Phase 2 behind pgvector availability) | Blocks + archival passages + FTS + auto-flush + cross-agent archives. |
| [`active/agent-to-agent-communication.md`](active/agent-to-agent-communication.md) | shipped (Phases 1–4) | Mention fanout, a2a conversation threading, sync AskAgent, sub-agent spawning with allow_delegation gate. |
| [`active/checkpoint-rollback.md`](active/checkpoint-rollback.md) | not started | Shadow-git commits per tool. Blocked on per-agent sidecar. |
| [`active/dashboard.md`](active/dashboard.md) | not started | Mobile-first SSE/HTMX observability. Read-only Phase 1 is low-friction. |
| [`active/json-state-migration.md`](active/json-state-migration.md) | shipped (Phases A + B) | session_map.json retired, state.json Phase B dual-writes routed through Postgres, file removed on startup when pool is set. |
| [`active/multi-agent-registry.md`](active/multi-agent-registry.md) | shipped (Phases 1–3) | Reconcile loop, spawn-time soul injection, `maquinista agent add/edit/archive/kill/spawn` CLI. |
| [`active/opencode-integration.md`](active/opencode-integration.md) | shipped (OC-01..06) | MonitorProfile, PlannerCommand role-framing, session-map fallback, LaunchCommand permission, model default, observed TUI profile. |
| [`active/per-agent-sidecar.md`](active/per-agent-sidecar.md) | not started | Rehomed from archived Task 1.7 — one sidecar goroutine per live agent, monitor tailing folded in, lease reaper. |
| [`active/retire-legacy-tmux-paths.md`](active/retire-legacy-tmux-paths.md) | partial | Rehomed from archived Task 1.9 — some legacy survives (`internal/queue/`, several `SendKeysWithDelay` call sites, the single-process `mailbox_consumer`). |
| [`active/resume-memory-refresh.md`](active/resume-memory-refresh.md) | not started | First-turn catch-up inject so `claude --resume <sid>` picks up memory / soul deltas written while the daemon was down. |
| [`active/pi-integration.md`](active/pi-integration.md) | not started | Add pi (`@mariozechner/pi-coding-agent`) as a fourth runner — `PiRunner`, `PiProfile`, `PiSource`. |
| [`active/productization-saas.md`](active/productization-saas.md) | not started | Multi-tenant SaaS turn — workspaces, RLS, billing, onboarding, pricing. Depends on dashboard auth. |

### Reference (design docs)

| Plan | Purpose |
|---|---|
| [`reference/maquinista-v2.md`](reference/maquinista-v2.md) | Core v2 architecture. §0 principle, §6 schema, §7 sidecar, §8 flows, appendices C (scheduled jobs + webhooks) and D (task-agents). Authoritative. |
| [`reference/maquinista_plan.md`](reference/maquinista_plan.md) | Foundational unified-binary plan that merged tramuntana + minuano. |
| [`reference/architecture-comparison.md`](reference/architecture-comparison.md) | Comparison to openclaw, tinyclaw, gas town/beads. Design rationale, not an implementation track. |

### Archive (done or obsolete)

| Plan | Why archived |
|---|---|
| [`archive/per-topic-agent-pivot.md`](archive/per-topic-agent-pivot.md) | Shipped in commits 3.19 / 3.20 / 3.21. Tier-3 spawns per-topic agents, `/agent_*` command family, migrations 013 + 014. |
| [`archive/maquinista-v2-implementation.md`](archive/maquinista-v2-implementation.md) | Task list. Phase 2 scheduler + Phase 3 task pipeline are shipped. Tasks **1.7** (per-agent sidecar) and **1.9** (retire legacy paths) were **not** fully delivered — rehomed into the active plans above. Task 3.6–3.7 cleanup audit outstanding. |
| [`archive/execution_plan.md`](archive/execution_plan.md) | Wave 1–5 shipped. Wave 6 Track A (spec parser) deferred; reintroduce if needed. |
| [`archive/minuano-turso-agentfs-migration.md`](archive/minuano-turso-agentfs-migration.md) | Obsoleted by §0 (Postgres as system of record). Kept as historical reference. |

## Suggested reading order for new contributors

1. `reference/maquinista-v2.md` §0 and §1–§2 — understand the substrate and coupling.
2. `reference/maquinista-v2.md` §6 (schema) and §8 (flows).
3. Skim `archive/per-topic-agent-pivot.md` to see how a pivot reads in this repo.
4. Pick an `active/*.md` to work on.
