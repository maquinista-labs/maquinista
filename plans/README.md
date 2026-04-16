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
| [`active/agent-soul-db-state.md`](active/agent-soul-db-state.md) | not started | DB-backed persona / identity. Unblocked. |
| [`active/agent-memory-db.md`](active/agent-memory-db.md) | not started | Persistent memory blocks + archives. Depends on soul. |
| [`active/agent-to-agent-communication.md`](active/agent-to-agent-communication.md) | partial (40%) | Mailbox schema landed; mention fan-out, `ask_agent`, sub-agent spawning pending. |
| [`active/checkpoint-rollback.md`](active/checkpoint-rollback.md) | not started | Shadow-git commits per tool. Blocked on per-agent sidecar. |
| [`active/dashboard.md`](active/dashboard.md) | not started | Mobile-first SSE/HTMX observability. Read-only Phase 1 is low-friction. |
| [`active/json-state-migration.md`](active/json-state-migration.md) | partial (40%) | Phase A schema shipped (migration 012); file readers/writers still around. |
| [`active/multi-agent-registry.md`](active/multi-agent-registry.md) | partial (50%) | Phase 1 reconcile loop shipped. Phase 2–3 (inject settings at spawn + `maquinista agent add/edit` CLI) pending. |
| [`active/opencode-integration.md`](active/opencode-integration.md) | partial (20%) | OC-01..04 (MonitorProfile, PlannerCommand, session tracking, LaunchCommand) all pending. |

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
| [`archive/maquinista-v2-implementation.md`](archive/maquinista-v2-implementation.md) | Phase 1–3 task list ~95% complete. Task 3.6–3.7 cleanup audit outstanding but no further planning needed. |
| [`archive/execution_plan.md`](archive/execution_plan.md) | Wave 1–5 shipped. Wave 6 Track A (spec parser) deferred; reintroduce if needed. |
| [`archive/minuano-turso-agentfs-migration.md`](archive/minuano-turso-agentfs-migration.md) | Obsoleted by §0 (Postgres as system of record). Kept as historical reference. |

## Suggested reading order for new contributors

1. `reference/maquinista-v2.md` §0 and §1–§2 — understand the substrate and coupling.
2. `reference/maquinista-v2.md` §6 (schema) and §8 (flows).
3. Skim `archive/per-topic-agent-pivot.md` to see how a pivot reads in this repo.
4. Pick an `active/*.md` to work on.
