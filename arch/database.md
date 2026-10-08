# Database

## Role

Postgres is the single source of truth for all persistent state. There
are no JSON files in the hot path (the legacy `state.json` and
`session_map.json` are retired). All coordination between the bot,
dashboard, relay, dispatcher, and scheduler flows through DB rows and
`NOTIFY` channels.

## Migration system

Migrations live in `internal/db/migrations/` as numbered SQL files
(`001_*.sql` … `NNN_*.sql`). `db.RunMigrations` applies them in order
using a `schema_migrations` table for tracking. Idempotent: already-
applied migrations are skipped.

`maquinista migrate` is the CLI command. Migrations run automatically at
daemon startup.

## NOTIFY channels

| Channel | Fired by | Consumed by |
|---------|----------|-------------|
| `agent_inbox_new` | INSERT trigger on `agent_inbox` | mailbox consumer, sidecars |
| `agent_outbox_new` | INSERT trigger on `agent_outbox` | relay |
| `channel_delivery_new` | INSERT trigger on `channel_deliveries` | dispatcher |
| `task_events` | various task status changes | orchestrator |

All consumers use `LISTEN` + a poll fallback (10 s) so a missed `NOTIFY`
(e.g. during reconnect) is caught on the next tick.

## Key tables

| Table | Purpose |
|-------|---------|
| `agents` | One row per agent; status, tmux_window, runner_type, workspace, last_transcript_at (liveness, MAQ-9), last_turn_end_at + turn_end_nudged (ADR-0008 completion contract) |
| `agent_souls` | Per-agent identity / system prompt fields |
| `soul_templates` | Reusable soul blueprints |
| `agent_memory` | Key/value memory blocks appended to soul render |
| `agent_inbox` | Inbound messages to agents |
| `agent_outbox` | Outbound responses from agents |
| `channel_deliveries` | Per-channel delivery rows fanned out by relay |
| `topic_agent_bindings` | Telegram topic → agent routing |
| `agent_workspaces` | Per-agent git worktrees / workspace records |
| `conversations` | Multi-turn conversation threads (A2A + human) |
| `tasks` | Orchestrator task graph |
| `ticket_issue_map` | Ticket issue ↔ task mirror; the row is the bridge claim |
| `ticket_comment_log` | Consumed ticket-comment ids (comment-verb exactly-once) |
| `merge_queue` | One row per merge attempt (pending → merging → merged\|conflict\|failed); terminal `conflict` rows also ledger parked tasks' merge-up budgets (MAQ-42) |
| `task_context` | Per-task journal (observations, verdicts, results, fix/salvage rows); freeze observations carry `cause` — `silent_success` (no respawn budget burned) vs `true_freeze` (counts against `MAQUINISTA_WATCHDOG_RESPAWN_CAP`), NULL = unclassified (ADR-0008) |
| `job_registry` | Scheduled jobs (cron + hooks) |
| `soul_templates` | Reusable soul templates |

## Connection pooling

`db.Connect` returns a `*pgxpool.Pool`. A single pool is shared across
all daemon goroutines. Each subsystem acquires connections from the pool
as needed; long-lived `LISTEN` connections use `pool.Acquire` to hold a
dedicated connection.

## TODO

- [ ] Document schema_migrations table
- [ ] Document backfill migrations (db/backfill.go)
- [ ] Write migration checklist for operators
- [ ] Connection pool sizing guidance
- [ ] plans/active/json-state-migration.md — completion status
