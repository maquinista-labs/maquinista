# Mailbox Architecture (v2)

> Goal: Give a contributor the runtime picture they need to reason about how a message turns into an agent action, how replies get back to the right topic, and where to plug in new skills, webhooks, or scheduled jobs.

This document is a reader-facing reference for the v2 architecture. It does **not** cover:

- Design rationale or rejected alternatives — see [`../plans/maquinista-v2.md`](../plans/maquinista-v2.md).
- Task-by-task rollout, feature-flag names, or migration ordering — see [`../plans/maquinista-v2-implementation.md`](../plans/maquinista-v2-implementation.md).

Some files and tables referenced here land across the three migrations described in the plan (`009_mailbox.sql`, `010_jobs.sql`, `011_task_pipeline.sql`). Forward-references are marked `[planned]` — the cited plan section is authoritative until the code ships.

---

## 1. Overview

v2 is an **α-only** architecture: every live agent runs inside an interactive pty with Claude (or another interactive runner), and a per-agent **sidecar** is the sole consumer of that agent's mailbox. Every path into an agent — humans in Telegram, webhook POSTs, scheduled cron fires, handoffs from other agents, task dispatch — reduces to a single primitive: **insert a row into `agent_inbox`**. Every path out flows the same way: **insert a row into `agent_outbox`**, the relay fans out into `channel_deliveries`, the dispatcher pushes to Telegram.

No filesystem-based comms (no `~/.tinyclaw/files/`, no `session_map.json`). Postgres is the substrate; `LISTEN/NOTIFY` is the wake signal; `FOR UPDATE SKIP LOCKED` keeps concurrent claim safe.

---

## 2. Topology at a glance

```
  Telegram topic                                              Telegram topic(s)
  ┌─────────┐                                                 ┌─────────────┐
  │ #project│───┐                                         ┌──▶│  owner +    │
  └─────────┘   │                                         │   │  observers  │
                │                                         │   │  + origin   │
  Cron fire ────┼──▶ agent_inbox ──▶ sidecar ──▶  pty  ──┐│   └─────────────┘
                │    (INSERT +     (claims + drives     ││
  Webhook POST ─┤     NOTIFY)       pty, tails JSONL)   ││          ▲
                │                                         │          │
  Agent mention │         │                               │          │
  from outbox ──┘         │                               ▼          │
                          │                        agent_outbox      │
                          │                          (INSERT +      │
                          │                           NOTIFY)       │
                          │                               │          │
                          │                               ▼          │
                          │                        outbox relay      │
                          │                  (fan-out to subscribers)│
                          │                               │          │
                          │                               ▼          │
                          │                      channel_deliveries ─┘
                          │                        (dispatcher → TG)
                          │
                          └── conversation_id threads agent-to-agent
                              handoffs across inbox/outbox pairs
```

Each arrow is a DB commit or an external side effect. `LISTEN/NOTIFY` channels wake consumers: `agent_inbox_new`, `agent_outbox_new`, `channel_delivery_new`, `agent_stop`, `task_events`.

---

## 3. The mailbox tables

All schema lives in `internal/db/migrations/009_mailbox.sql` [planned — see `plans/maquinista-v2.md` §6]. Existing tables from migration `001_initial.sql` (`tasks`, `task_deps`, `task_context`, `agents`) are reused unchanged.

### 3.1 `agent_inbox`

One row per message addressed to an agent. The sidecar claims pending rows with `FOR UPDATE SKIP LOCKED`, takes a 5-minute lease, drives the pty, then acks.

| Column | Purpose |
|---|---|
| `agent_id` | target agent (FK to `agents`) |
| `from_kind` | `user` \| `agent` \| `system` \| `scheduled` \| `webhook` |
| `origin_channel`, `origin_user_id`, `origin_thread_id`, `origin_chat_id` | reply target — where the agent's response fans out to (see §5) |
| `external_msg_id` | Telegram `update_id`, `sched:<job>:<ts>`, or `hook:<handler>:<delivery_id>` — dedupe key via `UNIQUE (origin_channel, external_msg_id)` |
| `content` JSONB | `{type:'text'\|'command', text, …}` |
| `status` | `pending` → `processing` → `processed` \| `failed` \| `dead` |
| `claimed_by`, `lease_expires` | worker id + lease for crash recovery |

NOTIFY: `agent_inbox_new` fires on INSERT and on status flips back to `pending` (lease expiry reaper). Payload is `agent_id`.

### 3.2 `agent_outbox`

One row per response or initiative from an agent. `in_reply_to` points back at the triggering inbox row (NULL for unsolicited outputs).

| Column | Purpose |
|---|---|
| `in_reply_to` | FK to `agent_inbox.id` — carries origin routing into §5 fan-out |
| `content` JSONB | `{parts:[{type,text\|image_ref}]}` |
| `mentions` JSONB | `[{agent_id, text}]` — extracted `[@agent_id: …]` handoffs, re-enqueued as new inbox rows |
| `status` | `pending` → `routing` → `routed` \| `failed` |

NOTIFY: `agent_outbox_new` on INSERT, payload is `outbox_id`.

### 3.3 `channel_deliveries`

One row per (outbox, subscriber) the relay produces. The dispatcher is the only reader.

| Column | Purpose |
|---|---|
| `binding_type` | `owner` \| `observer` \| `origin` — where this subscriber came from (see §5) |
| `channel`, `user_id`, `thread_id`, `chat_id` | fully-qualified Telegram target |
| `status` | `pending` → `sending` → `sent` \| `failed` \| `skipped` |
| `external_msg_id` | Telegram message id returned by the send API |

`UNIQUE (outbox_id, channel, user_id, thread_id)` dedupes when the same topic appears twice (e.g., origin also owns the topic). NOTIFY: `channel_delivery_new`.

### 3.4 `agent_topic_sessions`

Per-`(agent_id, user_id, thread_id)` runner session ID. Replaces `session_map.json`. Written by the SessionStart hook (or the sidecar's fallback for hookless runners like OpenCode). `reset_flag` lets `/reset` in Telegram ask the next turn to start a fresh runner session.

> **Deferred under the per-topic-agent pivot** (`plans/per-topic-agent-pivot.md`). With agents now 1:1 with topics, the `(user_id, thread_id)` part of the PK and `reset_flag` are not read by the routing layer. The table is kept intact for the forthcoming session-resume plan, which will tighten the PK and decide the fate of `reset_flag`.

### 3.5 `agent_settings`

Per-agent config: `persona`, `system_prompt`, `heartbeat`, `roster`. Used by the sidecar to seed the system prompt on turn entry.

> The earlier `is_default BOOLEAN` column and its partial unique index (`WHERE is_default`) were dropped in migration 013 (per `plans/per-topic-agent-pivot.md`). Tier-3 routing no longer consults a global default; it spawns a fresh per-topic agent instead.

### 3.6 `topic_agent_bindings`

Carry-over from migration `007_topic_observations.sql`, extended in 009 with `user_id`, `thread_id`, `chat_id`. `binding_type IN ('owner','observer')`; channel_deliveries additionally accepts `'origin'` for the ad-hoc reply-to-sender case. A `(user_id, thread_id)` has at most one `owner` (partial unique index); observers are unconstrained per agent.

---

## 4. Routing an unaddressed message

When Telegram hands the bot a message, it resolves the target agent using a **4-tier ladder** (`plans/maquinista-v2.md` §8.1). First hit wins.

| Tier | Rule | Side effect |
|------|------|-------------|
| 1 | Explicit `@<id-or-handle>` prefix in `m.text` | strip mention, resolve against `agents.id` or `agents.handle` (case-insensitive), use that agent; **no binding written** |
| 2 | `topic_agent_bindings` row with `(user_id, thread_id, binding_type='owner')` | none — steady state |
| 3 | **Spawn a fresh per-topic agent** via `SpawnTopicAgent` (id `t-<chat_id>-<thread_id>`) | write the owner binding so tier 2 takes over next turn |
| 4 | `/agent_default @handle` (explicit attach) | user's selection writes the owner binding |

```
if m.text matches /^@([A-Za-z0-9][A-Za-z0-9_-]*)/:
    token = captured
    SELECT id FROM agents WHERE id=$token OR LOWER(handle)=LOWER($token) LIMIT 1;
    agent_id = resolved; strip mention
else:
    SELECT agent_id FROM topic_agent_bindings
      WHERE user_id=$u AND thread_id=$t AND binding_type='owner';
    if not found:
        agent_id = SpawnTopicAgent($u, $t, $c, cwd, runner)
                 = 't-' || $c || '-' || $t
        INSERT INTO topic_agent_bindings (…, binding_type='owner');
INSERT INTO agent_inbox (…) ON CONFLICT DO NOTHING;
```

Tiers deliberately **not** present (vs. tinyclaw): static `settings.json` mapping, topic-name fuzzy match, silent "first available" fallback. The DB binding table owns all persistent routing state.

---

## 5. Fan-out on reply

When the outbox relay picks up a pending outbox row, it produces `channel_deliveries` rows from **three** sources (`plans/maquinista-v2.md` §8.2), UNIONed and deduped by the unique index:

1. **Origin** (`binding_type='origin'`) — the triggering inbox row's `origin_*` columns, if `origin_channel='telegram'`. This covers tier-1 `@mentions` from unbound topics where no binding exists yet.
2. **Owner** — the agent's single owner topic in `topic_agent_bindings`.
3. **Observers** — every `binding_type='observer'` row for this agent.

Mentions in the outbox row's `mentions` JSONB are separately re-enqueued as new inbox rows for the target agent(s), carrying the same `conversation_id` so follow-up replies aggregate in the right thread.

---

## 6. Job sources

Four ways to put a row into `agent_inbox`:

### 6.1 Telegram topics (the baseline)

Covered in §4. `from_kind='user'`. The `bot` package handles ingress; ingress writes inbox rows behind a feature flag during rollout (Phase 1 of the implementation plan).

### 6.2 Scheduled jobs (Appendix C.2 surface)

`scheduled_jobs` table [planned — migration 010] holds `(cron_expr, timezone, agent_id, prompt, reply_channel, warm_spawn_before, next_run_at)`. A single-replica `maquinista scheduler` daemon claims due rows, runs `orchestrator.ensure_agent` if the target isn't live, and enqueues an inbox row with `from_kind='scheduled'` and `external_msg_id = 'sched:<job_id>:<fire_ts>'` for idempotent re-fire.

Cron semantics: **missed fires collapse to a single catch-up** (outages longer than one period fire once, not N times). "Run now" is `UPDATE scheduled_jobs SET next_run_at = NOW()`.

### 6.3 Webhook handlers (Appendix C.3 surface)

`webhook_handlers` table [planned — migration 010] holds `(path, secret, signature_scheme, event_filter, agent_id, prompt_template, reply_channel, rate_limit_per_min)`. `maquinista webhook-serve --addr :8080` [planned] receives POSTs, verifies HMAC, applies `event_filter` as a `jsonb_path_match` predicate, templates the prompt against the payload, and enqueues with `from_kind='webhook'` and `external_msg_id = 'hook:<handler_id>:<delivery_id>'`.

Auth is non-negotiable: per-handler secret, HMAC verification on every POST, replay-safe via the unique-index dedupe, 1MB default body cap. Multiple replicas are safe behind a load balancer — DB idempotency is the source of truth.

### 6.4 Task pipeline (Appendix D surface — planner → `@impl-<task_id>` → `@reviewer` → `@pr-closer`)

`plans/maquinista-v2.md` Appendix D wires the existing planner + task DAG (migrations 001, 004, 008) into the mailbox without modifying those tables beyond migration 011's three columns (`worktree_path`, `pr_url`, `pr_state`) and a partial unique index that enforces "at most one live agent per task."

Flow (abbreviated):

```
planner writes tasks → refresh_ready_tasks trigger cascades → ready
  → task-scheduler daemon claims, ensure_agent(@impl-<task_id>),
    INSERT agent_inbox (@impl-<task_id>, '/work-on-task <id>')
  → implementor runs `gh pr create`, sets pr_url + status='review'
  → github pull_request.opened webhook → @reviewer (/review-pr <n>)
  → github pull_request.merged webhook → @pr-closer (/close-pr <n>)
    @pr-closer sets status='done' → refresh_ready_tasks cascades deps
    UPDATE agents SET status='dead', stop_requested=TRUE WHERE id='@impl-<id>'
```

`@impl-<task_id>` is per-task: minted on dispatch, retired on merge. The PR-lifecycle webhooks go through dedicated agents (not direct SQL) so every event produces narration into the originating `#project` topic via observer binding.

---

## 7. Agent lifecycle

### 7.1 Lazy spawn

Inbox drives the agent into existence. If an inbox row addresses an agent whose pty isn't live, the scheduler / webhook / task dispatcher calls `orchestrator.ensure_agent` before enqueueing. For cron, `warm_spawn_before` can pre-spawn so the first turn doesn't pay the boot cost.

### 7.2 Per-task agents

For the task pipeline (§6.4), `ensure_agent(role, task_id)` mints `@impl-<task_id>`, creates the `agents` row with `(task_id=t.id, role=t.role, status='working')`, starts the sidecar with `working_dir = t.worktree_path`, and attaches the originating `#project` topic as an `observer`. `uq_agents_task_live` (migration 011 [planned]) enforces one live agent per task.

### 7.3 Session continuity

`agent_topic_sessions(agent_id, user_id, thread_id, runner, session_id, reset_flag)` persists the runner's native session UUID. SessionStart hooks (Claude) or the sidecar fallback (OpenCode — see `64759a6`) write this row; the next message for that `(agent, topic)` tuple resumes the session unless `reset_flag` was set by `/reset`.

### 7.4 Retirement

`agents.stop_requested BOOLEAN` is the soft-stop signal; the `agent_stop` NOTIFY channel wakes the sidecar, which exits cleanly. For per-task agents, the `@pr-closer` skill sets `stop_requested=TRUE` on merge, the sidecar exits, the tmux pane is torn down, and the agent's `observer` binding is cleaned up.

Lease expiry reaper: rows stuck in `status='processing'` past `lease_expires` flip back to `pending`, triggering `agent_inbox_new` so a fresh sidecar (or the recovered one) can re-claim.

---

## 8. Observability

### 8.1 Slash commands

| Command | Reads | Purpose |
|---|---|---|
| `/agent_list` | `agents` | list all registered agents |
| `/agent_default @handle` | `topic_agent_bindings (owner)` | attach this topic to an existing agent (unknown handle errors) |
| `/agent_rename <handle>` | `agents.handle` | set a friendly alias on the current topic's agent |
| `/observe @handle` | `topic_agent_bindings (observer)` | add this topic as an observer (when implemented) |
| `/jobs` | `scheduled_jobs` | list registered cron jobs |
| `/hooks` | `webhook_handlers` | list registered webhook endpoints |
| `/job-runs <name>` | `job_runs` view | last N executions of a job or hook |

### 8.2 `job_runs` view

Unifies both programmatic sources:

```sql
CREATE VIEW job_runs AS
SELECT i.id AS inbox_id, i.from_kind, i.from_id AS source_id,
       i.agent_id, i.enqueued_at, i.processed_at, i.status, i.last_error,
       o.id AS outbox_id, o.content AS agent_response
FROM agent_inbox i
LEFT JOIN agent_outbox o ON o.in_reply_to = i.id
WHERE i.from_kind IN ('scheduled','webhook');
```

### 8.3 Tracing a message that disappeared

When a Telegram message got no response, walk the pipeline in order:

1. `SELECT * FROM agent_inbox WHERE origin_channel='telegram' AND external_msg_id=<update_id>` — was it enqueued at all? If missing, check the routing ladder: did tier-4 picker pop and never get clicked? Is there a default agent?
2. If present but `status='pending'`: sidecar isn't running or isn't claiming. Check `agents.status`, `agents.stop_requested`, and the `agent_inbox_new` listener.
3. If `status='processing'` past the lease: sidecar died mid-turn. The reaper will flip it back to `pending`; if it doesn't, the reaper itself isn't running.
4. If `status='processed'`: check `agent_outbox WHERE in_reply_to=<inbox.id>`. Missing → agent produced no response (check JSONL transcript).
5. If outbox exists but `status='pending'`: relay isn't running.
6. If outbox is `routed` but no `channel_deliveries`: the triggering inbox row had no `origin_*` and the agent has no owner/observer bindings for Telegram.
7. If `channel_deliveries` is `failed` or `sending` forever: dispatcher / Telegram API issue; `last_error` is populated.

---

## 9. Extending the system

### 9.1 Add a new agent

1. Easiest: send a message in a fresh Telegram topic. Tier-3 of the routing ladder will spawn a fresh agent (`t-<chat_id>-<thread_id>`), tmux window, and Claude process automatically.
2. Optionally run `/agent_rename <handle>` in that topic to give it a friendly alias you can mention from other topics.
3. `INSERT INTO agent_settings (agent_id, persona, system_prompt, heartbeat, roster)` to tune persona/prompt.
4. For a bespoke runner, add to `internal/runner/` (`InteractiveCommand` only — the non-interactive surface is being removed per §10a of the plan).

### 9.2 Add a new skill

Skills are inbox-driven. A slash command in the agent's pty (e.g. `/review-pr 42`) is just text in `agent_inbox.content.text`; the agent's runtime interprets it. To add `/foo`:

- Put the skill definition where the runner expects it (for Claude: `~/.claude/skills/foo.md` in the agent's working dir).
- Reference it from the agent's `system_prompt` in `agent_settings`.
- No Go-side hook needed.

### 9.3 Add a new webhook handler

```
maquinista webhooks add \
  --name deploy-notify --path /hooks/deploy \
  --agent @deployer --signature generic-hmac \
  --prompt '/handle-deploy {{.service}} {{.version}}' \
  --reply-to 'telegram:chat=...,thread=...'
```

[planned — CLI and underlying `webhook_handlers` row land in migration 010]. Pick a strong secret, set `rate_limit_per_min`, and optionally pass `--event-filter '$.action == "completed"'` as a `jsonb_path_match` predicate.

### 9.4 Add a new scheduled job

```
maquinista schedule add \
  --name daily-report --cron '0 8 * * *' --tz UTC \
  --agent @reporter --prompt '/daily-report' \
  --reply-to '#ops-reports' --warm-spawn 1m
```

[planned — migration 010]. Declarative YAML under `config/schedules/*.yaml` reconciled on startup is an alternative path.

---

## 10. Relationship to legacy components

| Legacy | Status under v2 |
|---|---|
| `internal/queue/queue.go` (per-user merge queue) | **Removed** — concerns fold into the dispatcher |
| `internal/state/session_map.go` (JSON file) | **Removed** — replaced by `agent_topic_sessions` |
| `state.ThreadBindings` (in-memory map) | **Removed** — read-through cache over `topic_agent_bindings` |
| `internal/monitor/` (pane scraper pushing to TG) | **Folded** into the sidecar; JSONL tail writes `agent_outbox` instead of sending |
| `tmux.SendKeysWithDelay` from `internal/bot/handlers.go` | **Removed** — ingress writes `agent_inbox` instead |
| Non-interactive runner surface (`RunNonInteractive`, `NonInteractiveArgs`, `-p <msg>`) | **Removed** — see `plans/maquinista-v2.md` §10a for the full inventory |
| `maquinista schedule` (existing cron, DAG-template-based) | **Retargeted** — still creates task rows, but their dispatch edge moves from "direct tmux spawn" to "insert `agent_inbox`" (§6.4) |
| volta planner + task DAG (migrations 001, 004, 008) | **Preserved unchanged** — Appendix D adds the dispatch edge; the planner itself keeps writing `tasks` and `task_deps` |
| `/t_plan` (see `planning-workflows.md` §1) | **Retargeted** — planner agent writes tasks via the typed `maquinista tasks` tool surface, not raw SQL |
| `hook/hook.go` (SessionStart writing `session_map.json`) | **Retargeted** — writes `agent_topic_sessions` |

---

## 11. Key files reference

| File | Role |
|------|------|
| `internal/db/migrations/001_initial.sql` | `tasks`, `agents`, `task_deps`, `task_context`, `refresh_ready_tasks` trigger |
| `internal/db/migrations/004_notify_triggers.sql` | `task_events` NOTIFY (reused by task-scheduler §6.4) |
| `internal/db/migrations/007_topic_observations.sql` | `topic_agent_bindings` (extended in 009) |
| `internal/db/migrations/008_agent_role.sql` | `agents.role` column (used by `ensure_agent(role, task_id)`) |
| `internal/db/migrations/009_mailbox.sql` | mailbox tables, NOTIFY triggers (§3). `is_default` dropped in 013. |
| `internal/db/migrations/013_drop_default_agent_flag.sql` | retires tier-3 global default (per-topic-agent-pivot.md) |
| `internal/db/migrations/014_agents_handle.sql` | `agents.handle` + case-insensitive unique index |
| `internal/db/migrations/010_jobs.sql` [planned] | `scheduled_jobs`, `webhook_handlers`, `job_runs` view (§6.2/6.3) |
| `internal/db/migrations/011_task_pipeline.sql` [planned] | `tasks.worktree_path`/`pr_url`/`pr_state`, `uq_agents_task_live` (§6.4) |
| `internal/bot/handlers.go` | Telegram ingress → `agent_inbox` INSERT (Phase 1 of impl plan) |
| `internal/agent/agent.go` | Spawn path; creates `agent_settings` row, starts sidecar |
| `internal/orchestrator/orchestrator.go` | Liveness (sidecar-driven), `ensure_agent(role, task_id)` |
| `internal/runner/{claude,opencode,openclaude,custom}.go` | Interactive-only runners (§10a cleanup removes `NonInteractive*`) |
| `internal/mailbox/` [planned] | Typed DB ops: enqueue/claim/ack/fail/fanout/attachments |
| `internal/sidecar/` [planned] | Per-agent pty bridge + JSONL tail → outbox |
| `internal/dispatcher/` [planned] | Telegram outbound from `channel_deliveries` |
| `cmd/maquinista/cmd_scheduler.go` [planned] | `maquinista scheduler` daemon (Appendix C.2) |
| `cmd/maquinista/cmd_webhook_serve.go` [planned] | `maquinista webhook-serve` HTTP ingress (Appendix C.3) |
| `cmd/maquinista/cmd_task_scheduler.go` [planned] | `maquinista task-scheduler` (§6.4 dispatch loop) |
| `cmd/maquinista/cmd_jobs.go`, `cmd_webhooks.go` [planned] | CLI for registering scheduled jobs and webhook handlers |
| `hook/hook.go` | SessionStart writes `agent_topic_sessions` |
| `plans/maquinista-v2.md` | authoritative design; §6 schema, §8 flows, Appendices C & D |
| `plans/maquinista-v2-implementation.md` | 21-task rollout plan, feature flags, testing plan |

---

## 12. Open questions

Inherited from `plans/maquinista-v2.md` §11; listed here so the docs reader sees them:

1. **Lease duration** — 5min is the starting default. Too short causes duplicate processing on slow turns; too long wastes time after a real crash. Revisit once there's production traffic to measure turn-length distribution.
2. **Attachment size threshold** — `BYTEA` to ~10MB, Large Objects beyond. Current plan picks 5MB; confirm with real payloads.
3. **Outbox retention** — `agent_inbox`, `agent_outbox`, `channel_deliveries` grow unbounded. Weekly archive job is sketched but unwritten (`docs/retention.md` TODO).
4. **Conversation aggregation close semantics** — `pending_count` decrement logic ports from tinyclaw; edge cases around agent timeouts / dropped handoffs aren't exercised yet.
5. **Agent crash recovery arbiter** — on sidecar restart, deciding between "retry" and "ack" for a `processing` row relies on the JSONL tail offset. The failure-mode matrix is in §11 of the plan but needs a first real incident to validate.
6. **DB as SPOF** — already true for the existing task tables; v2 widens the surface. HA mitigation (replication, failover) is out of scope for this doc.
