# Maquinista v2 — Sequential Implementation Plan

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

Derived from `plans/maquinista-v2.md` (§10 migration path, Appendices C and D). Tasks execute **strictly sequentially** — each task's deliverable gates the next one's start. Every task carries a feature flag so production traffic can be cut over gradually; each task's testing section assumes the flag is OFF for live users until verification is complete.

**Phase map:**

- **Phase 1 — Core mailbox** (tasks 1.1 – 1.9): migration 009, α sidecar, retire legacy tmux dispatch.
- **Phase 2 — Programmatic job sources** (tasks 2.1 – 2.5): migration 010, scheduler daemon, webhook server.
- **Phase 3 — Task pipeline** (tasks 3.1 – 3.7): migration 011, per-task agents, PR lifecycle, §10a cleanup.

All schema SQL lives under `internal/db/migrations/`. All tests live beside the code they cover; end-to-end integration tests go under `test/e2e/` and use `testcontainers-go` Postgres.

---

## Phase 1 — Core mailbox

### Task 1.1 — Migration 009: mailbox schema

**Depends on:** nothing (first task).

**Deliverable:** `internal/db/migrations/009_mailbox.sql` containing the tables and triggers from `plans/maquinista-v2.md` §6 — extended `topic_agent_bindings`, `agent_topic_sessions`, `conversations`, `agent_inbox`, `agent_outbox`, `channel_deliveries`, `message_attachments`, `agent_settings` (with `is_default BOOLEAN`), NOTIFY triggers for `agent_inbox_new` / `agent_outbox_new` / `channel_delivery_new` / `agent_stop`, and `ALTER TABLE agents ADD COLUMN stop_requested BOOLEAN`. Backfill script moves `state.ThreadBindings` JSON into `topic_agent_bindings`.

**Testing:**

- `go test ./internal/db/...` — migration applies cleanly against a fresh Postgres (testcontainers).
- Backfill idempotency: run twice, assert row counts stable.
- NOTIFY: unit test opens a second connection, issues `LISTEN agent_inbox_new`, inserts a row, verifies the payload matches `agent_id`.
- Constraint check: `UNIQUE (user_id, thread_id) WHERE binding_type='owner'` rejects a second owner for the same topic.

### Task 1.2 — `internal/mailbox/` package

**Depends on:** 1.1.

**Deliverable:** typed Go wrappers covering `EnqueueInbox`, `ClaimInbox`, `AckInbox`, `FailInbox`, `AppendOutbox`, `ClaimOutbox`, `AckChannelDelivery`, `InsertAttachment` (BYTEA + large-object branch ≥5 MB). All ops take a `*sql.Tx` so callers control the transaction.

**Testing:**

- Unit tests for each op against testcontainers Postgres.
- Concurrent claim test: spawn 4 goroutines claiming from the same agent's inbox; assert zero duplicate claims via `FOR UPDATE SKIP LOCKED`.
- Lease expiry test: claim a row, advance `pg_sleep`-mocked clock past `lease_expires`, assert a second claim succeeds.
- Idempotency: enqueue two rows with the same `(origin_channel, external_msg_id)`; assert `ON CONFLICT DO NOTHING` collapses to one.

### Task 1.3 — Outbox relay daemon

**Depends on:** 1.2.

**Deliverable:** `internal/relay/` + a `maquinista relay` subcommand. `LISTEN agent_outbox_new`, claim pending outbox rows, in one TX: insert origin-fanout + binding-fanout `channel_deliveries` rows (§8.2 SQL), parse `[@agent_id: …]` mentions into new `agent_inbox` rows, update the outbox row to `routed`.

**Testing:**

- Unit test for the fan-out SQL with a crafted `agent_outbox` row; assert correct number of `channel_deliveries` and `agent_inbox` rows with right keys.
- Mention parser table-driven tests (malformed, nested, escaped).
- Crash recovery: kill the daemon mid-TX, assert rows remain `pending`; restart and watch them route exactly once.
- No deliveries to self when agent's own topic is bound as observer (UNIQUE `(outbox_id, channel, user_id, thread_id)` covers it).

### Task 1.4 — Telegram dispatcher

**Depends on:** 1.3.

**Deliverable:** `internal/dispatcher/` + a `maquinista dispatch` subcommand. `LISTEN channel_delivery_new`, claim pending rows, call the existing Telegram Bot API client, `UPDATE status='sent'` with the returned `message_id`. On 429, write `status='pending', enqueued_at = NOW() + '30s'`. On other failure, bump `attempts`; exceed `max_attempts` → `status='failed'`.

**Testing:**

- Mock Telegram client; assert `SendMessage(chat_id, thread_id, content)` arguments match the `channel_deliveries` row.
- 429 path: mocked 429 → row returns to `pending` with correct reschedule time.
- Max-attempts path: force failures → final status `failed` with `last_error` populated.
- Rate-limit: spam 200 rows for one chat, verify dispatcher-side pacing keeps under Telegram's 30 msg/s ceiling.

### Task 1.5 — Monitor emits to `agent_outbox`

**Depends on:** 1.4.

**Deliverable:** modify `internal/monitor/source_claude.go` to write every captured response to `agent_outbox` in addition to today's direct Telegram send path. No functional change for users; the new path is passive.

**Testing:**

- Start an agent, message it through the existing legacy path, then assert a matching `agent_outbox` row exists. Compare to the legacy-delivered Telegram message for content parity.
- Fuzz: ANSI sequences, image attachments, huge transcripts — verify outbox rows never truncate content or drop attachments.
- Run both paths against a staging topic for 24h; diff legacy Telegram messages vs. outbox-derived messages (post-dispatch from task 1.4 running in shadow mode). Zero divergence gates the next task.

### Task 1.6 — Bot writes to `agent_inbox` (flag-gated, with bridge)

**Depends on:** 1.5.

**Deliverable:** in `internal/bot/handlers.go`, behind a per-topic feature flag, write inbound Telegram messages to `agent_inbox` instead of calling `tmux.SendKeysWithDelay` directly. Ship a minimal in-process bridge that `LISTEN`s on `agent_inbox_new` and drives the pty via `SendKeysWithDelay` — so α is live without sidecar separation yet.

**Testing:**

- Integration test: bot receives a fake update → one row in `agent_inbox` with the right `external_msg_id` → bridge wakes → tmux `send-keys` called with correct window + text.
- Idempotency: replay the same Telegram `update_id` twice → exactly one agent turn.
- Flag off: message routes via legacy path; flag on: message routes via inbox path. Both paths hit the same pane; outputs are indistinguishable.
- Flip flag on a single staging topic for a week; monitor inbox metrics for stuck rows.

### Task 1.7 — Extract sidecar into `internal/sidecar/`

**Depends on:** 1.6.

**Deliverable:** `internal/sidecar/` package providing `SidecarRunner` that owns (a) the pty driver and (b) the JSONL transcript tail for one agent. Orchestrator spawns one sidecar goroutine per live agent on `orchestrator.spawn`. `internal/monitor/` collapses into the sidecar — the standalone monitor process goes away.

**Testing:**

- Unit tests for the sidecar's claim/drive/ack loop using a fake runner that writes a scripted JSONL transcript.
- Crash: kill the sidecar mid-turn via `SIGTERM`; assert the row stays `processing` with `claimed_by`; restart supervisor; assert claim-expiry reclaim replays to `pending`.
- Lease expiry: set lease to 5s in test, stall the fake runner, assert reaper returns row to `pending` exactly once.
- Parity with task 1.5's shadow mode: sidecar-produced outbox rows match legacy-monitor-produced outbox rows byte-for-byte.

### Task 1.8 — Routing ladder (§8.1)

**Depends on:** 1.7.

**Deliverable:** in bot handler, implement the four-tier ladder per `per-topic-agent-pivot.md`:

1. **Explicit mention** — `@id-or-handle` matches `agents.id` or `agents.handle` (the nullable user-assigned alias).
2. **Topic owner binding** — `topic_agent_bindings` with `binding_type='owner'`.
3. **Spawn new per-topic agent** — calls `SpawnTopicAgent(user, thread, chat, cwd, runner)` which inserts an `agents` row with id `t-<chat_id>-<thread_id>`, spawns a tmux window + Claude process, and writes the owner binding.
4. **Explicit attach via `/agent_default @handle`** — attaches current topic to an already-running agent. Unknown handle returns a guidance error; never auto-spawns.

Add `/agent_rename <handle>` to set the handle on the current topic's owner agent (regex `^[a-z0-9_-]{2,32}$`, reserved prefix `t-` forbidden, unique when set). Rename `/agents` → `/agent_list`. Hard cutover — no legacy aliases. `/global-default` is removed entirely (the `agent_settings.is_default` column is dropped in migration 013).

**Testing:**

- Table-driven tests for each tier: message, existing-state fixtures, expected binding writes + inbox agent_id.
- Tier-3 spawn test: mock `SpawnFunc`; assert it's called exactly once per fresh `(user, thread)`; assert the returned id receives the owner binding.
- Mention resolution: `@id` and `@handle` both resolve to the same canonical `agents.id`.
- `/agent_default @unknown` returns the guidance error and does not spawn.
- `/agent_rename` validation: accepts valid handles, rejects regex violations, reserved prefix `t-`, and uniqueness collisions.
- Concurrency: two humans in the same topic racing tier-3 spawn → partial unique index forces one winner; second request reads the committed row and proceeds.

### Task 1.9 — Retire legacy paths

**Depends on:** 1.8, plus a week of clean shadow-mode metrics from task 1.7.

**Deliverable:** delete (a) `internal/queue/queue.go`, (b) `state.ThreadBindings` in-memory map + `session_map.json` reader/writer, (c) direct `tmux.SendKeysWithDelay` calls from `internal/bot/handlers.go`, (d) the in-process bridge from task 1.6. Move session-id writes into `agent_topic_sessions` via `hook/hook.go`.

**Testing:**

- `go test ./...` green.
- `go vet ./...` clean.
- End-to-end: fresh staging env, bring up bot + relay + dispatcher + sidecars, run a 100-message interaction sequence across 3 topics and 2 agents; zero errors, zero lost messages, zero duplicates.
- Observability check: `SELECT count(*) FROM agent_inbox WHERE status='processing' AND claimed_at < NOW() - INTERVAL '10 min'` returns 0.

---

## Phase 2 — Programmatic job sources

### Task 2.1 — Migration 010: `webhook_handlers` + `scheduled_jobs`

**Depends on:** 1.9.

**Deliverable:** `internal/db/migrations/010_job_sources.sql` creating `webhook_handlers` and `scheduled_jobs` per Appendix C.2 and C.3, plus `ALTER TABLE agent_inbox DROP/ADD CONSTRAINT` widening `from_kind` to include `scheduled` and `webhook`.

**Testing:**

- Schema applies and rolls back cleanly (if rollback scripts are part of the convention; otherwise forward-only).
- Partial unique index on `webhook_handlers(path) WHERE enabled` rejects duplicates.
- `from_kind` CHECK rejects unknown values.

### Task 2.2 — Scheduler daemon

**Depends on:** 2.1.

**Deliverable:** `maquinista scheduler` subcommand (single replica). Loop: claim due `scheduled_jobs` with `FOR UPDATE SKIP LOCKED`, enqueue `agent_inbox` with `from_kind='scheduled'` and `external_msg_id='sched:<job_id>:<fire_ts>'`, compute `next_run_at` via `robfig/cron`, update the row. Respect `warm_spawn_before` by calling `orchestrator.ensure_live(agent_id)` early.

**Testing:**

- Unit test the cron `nextAfter` wrapper with DST transitions + fixed clock.
- Missed-fire behavior: job last ran 3 hours ago, cron was `*/30 * * * *`, assert exactly one inbox row enqueued on recovery (not six catch-up rows).
- Idempotency under restart: kill the scheduler between claim and inbox insert; assert next run picks up the same `next_run_at` and `ON CONFLICT DO NOTHING` on `external_msg_id` prevents double-send.
- Warm spawn: assert sidecar spawned `warm_spawn_before` ahead of `next_run_at`.

### Task 2.3 — Webhook HTTP server

**Depends on:** 2.2.

**Deliverable:** `maquinista webhook-serve --addr :8080` subcommand. Routes `POST /hooks/*` to registered `webhook_handlers`, verifies HMAC per `signature_scheme` (start with `github-hmac-sha256`), renders `prompt_template` via `text/template` against the JSON payload, enqueues `agent_inbox` with `from_kind='webhook'` and `external_msg_id='hook:<handler_id>:<delivery_id>'`. Per-handler token-bucket rate limit, 1 MB body cap.

**Testing:**

- Signed-request test: real GitHub signature fixture → 202 with inbox_id. Wrong signature → 401.
- Event filter test: handler filters `pull_request.opened` only; `push` event returns 204.
- Replay protection: same `X-Delivery-Id` twice → second request returns 202 but no new inbox row (idempotent).
- Rate limit: 61 requests in a minute → 61st returns 429.
- HA: run two `webhook-serve` replicas behind nginx; send 1000 requests split across both; assert exactly 1000 inbox rows.
- Size cap: 2 MB body → 413.

### Task 2.4 — Job registration surface

**Depends on:** 2.3.

**Deliverable:** slash commands (`/schedule`, `/hook-register`, `/hook-enable`, `/hook-disable`) and `maquinista schedule|hook add/list/rm` CLI. YAML reconcile at startup for `config/schedules/*.yaml` and `config/hooks/*.yaml` (declarative mode).

**Testing:**

- CLI round-trip: `add` → `list` shows it → `rm` removes it.
- Slash command round-trip: `/schedule daily-reels "0 8 * * *" @creator "/publish-reel"` → row present in `scheduled_jobs`.
- YAML reconcile: write a schedule file, restart, assert row appears; delete file, assert row disabled (soft-delete, not destroyed, to keep audit trail).
- Input validation: bad cron expressions rejected with a clear error before DB write.

### Task 2.5 — `job_runs` view + observability commands

**Depends on:** 2.4.

**Deliverable:** the `job_runs` SQL view from Appendix C.5, plus `/jobs`, `/hooks`, `/job-runs <name>` slash commands reading through it.

**Testing:**

- Join correctness: execute a scheduled job → `/job-runs <name>` shows one row with inbox + outbox.
- Failed job shows `status='failed'` and `last_error`.
- Pagination: 200 historical runs, `/job-runs <name>` pages 25 at a time.

---

## Phase 3 — Task pipeline (Appendix D)

### Task 3.1 — Migration 011: task pipeline schema

**Depends on:** 2.5.

**Deliverable:** `internal/db/migrations/011_task_pipeline.sql` with `ALTER TABLE tasks ADD COLUMN worktree_path TEXT, pr_url TEXT, pr_state TEXT CHECK (pr_state IN ('open','merged','closed'))`, `CREATE INDEX idx_tasks_pr_url`, `CREATE UNIQUE INDEX uq_agents_task_live ON agents(task_id) WHERE task_id IS NOT NULL AND status != 'dead'`, and a `COMMENT ON COLUMN tasks.status` documenting the widened state machine (adds `review`).

**Testing:**

- Migration applies; existing rows unaffected.
- Unique-live index prevents inserting a second `working` agent with the same `task_id`.
- PR-URL index is queryable: `EXPLAIN` shows index usage for `WHERE pr_url = $1`.

### Task 3.2 — `internal/tools/tasks/` typed surface

**Depends on:** 3.1.

**Deliverable:** a small Go package exposing typed ops — `CreateTask`, `AddDep`, `SetPRUrl`, `MarkReview`, `MarkMerged`, `MarkClosed`, `ValidateDAG` (cycle check via `WITH RECURSIVE`). Exposed to agents via MCP or a `maquinista tasks` CLI wrapper invoked from skills.

**Testing:**

- Cycle detection: build a 3-node cycle → `ValidateDAG` returns an error; a valid DAG returns nil.
- `CreateTask` enforces non-empty `title`, `worktree_path` when `role='implementor'`.
- `MarkMerged` flips `pr_state`, `status`, and relies on migration-001's `refresh_ready_tasks` trigger to unblock dependents — integration test asserts dependents flip to `ready`.

### Task 3.3 — `orchestrator.ensure_agent(role, task_id)`

**Depends on:** 3.2.

**Deliverable:** new entry point on the orchestrator. Mints `@impl-<task_id>` (alias-bumped on retry: `@impl-<task_id>-r2`), inserts `agents` row with `(task_id, role, status='working', stop_requested=FALSE)`, spawns the pty + sidecar with `working_dir = t.worktree_path`, attaches the task's originating topic as `observer` in `topic_agent_bindings`.

**Testing:**

- Unit test: call `ensure_agent(role='implementor', task_id='T-42')` against a test DB → agents row present, sidecar goroutine running, observer binding exists.
- Concurrency: two simultaneous calls for the same task_id → unique-live index causes one to fail cleanly; caller retries and the already-live agent is returned.
- Worktree missing: fail loudly; don't leak a half-spawned pane.

### Task 3.4 — Task scheduler daemon

**Depends on:** 3.3.

**Deliverable:** `maquinista task-scheduler` subcommand. `LISTEN task_events` (from migration 004), claim rows `WHERE status='ready' AND NOT EXISTS (SELECT 1 FROM agents WHERE task_id=t.id AND status!='dead')` with `FOR UPDATE SKIP LOCKED`, call `orchestrator.ensure_agent`, enqueue inbox with `{type:'task', task_id, prompt:'/work-on-task <id>'}`, flip `tasks.status='claimed'` + `claimed_by='@impl-<id>'`.

**Testing:**

- Ready-task → agent spawned + inbox enqueued + `tasks.status='claimed'` — all in one verified sequence.
- DAG flow: create `A → B → C`, complete A (manual `UPDATE`), B becomes ready, scheduler dispatches B, complete B, C dispatches next.
- Crash mid-dispatch: daemon killed after `ensure_agent` but before inbox insert → next tick detects (agent live, inbox empty) and enqueues the inbox message to heal.
- Concurrency: two task-scheduler replicas → `FOR UPDATE SKIP LOCKED` + unique-live index means each task is dispatched exactly once.

### Task 3.5 — PR-lifecycle skills + webhook handlers

**Depends on:** 3.4.

**Deliverable:** three skills under `~/.claude/skills/` shipped with the project — `/work-on-task <id>`, `/review-pr <n>`, `/close-pr <n>`. Two `webhook_handlers` rows (via `config/hooks/*.yaml`) — `github-pr-opened → @reviewer`, `github-pr-merged-or-closed → @pr-closer`. `/work-on-task` calls `internal/tools/tasks.SetPRUrl` once `gh pr create` returns a URL; `/close-pr` calls `MarkMerged` or `MarkClosed` based on payload.

**Testing:**

- Skill smoke: manually invoke `/work-on-task T-42` in a test agent pane, assert agent opens a PR in a scratch repo, `tasks.pr_url` populated, `tasks.status='review'`.
- Webhook handler smoke: POST a fixture GitHub `pull_request.opened` payload → `@reviewer` inbox row with rendered prompt.
- End-to-end happy path: human in `#project` topic says "plan: rename util X" → planner writes 2 tasks → task-scheduler dispatches → implementor opens PR → reviewer approves → merge webhook fires → `@pr-closer` marks done → dependent task dispatches. All within a single 30-minute integration run against a scratch GitHub repo.

### Task 3.6 — Retire volta-era direct task-dispatch path

**Depends on:** 3.5, plus a week of clean metrics from task 3.5.

**Deliverable:** delete the legacy "orchestrator spawns a task's agent directly into a tmux window and sends keys" code path. All task dispatch now flows through `agent_inbox`.

**Testing:**

- Full regression suite green.
- Dry-run: grep the codebase for `tmux.SendKeys.*task` patterns; confirm only sidecar usages remain.
- Stress: dispatch 50 tasks in parallel (bounded by agent process limits); all complete; no stuck rows.

### Task 3.7 — Execute §10a cleanup

**Depends on:** 3.6.

**Deliverable:** remove the non-interactive runner surface listed in `plans/maquinista-v2.md` §10a — `Runner.NonInteractiveArgs`, `Runner.RunNonInteractive`, all per-runner implementations (claude/opencode/openclaude/custom), `CustomRunner.NonInterTpl`, and the corresponding `TestXXX_NonInteractive*` tests. Keep `InteractiveCommand` only. Small PR per runner wrapper for easy review.

**Testing:**

- `go build ./...` and `go test ./...` green after each PR.
- Grep confirms zero remaining references to the removed identifiers.
- Check §10a of `plans/maquinista-v2.md`: mark each row done or delete.

---

## Cross-phase testing plan

Three test surfaces run throughout:

1. **Unit tests** next to each package (`*_test.go`) — covered per task above.
2. **Integration tests** under `test/e2e/` using `testcontainers-go` Postgres and a mock Telegram Bot API. Each phase adds a scenario file:
   - `test/e2e/phase1_mailbox_roundtrip_test.go` — bot → inbox → sidecar → outbox → dispatcher → Telegram.
   - `test/e2e/phase2_jobs_test.go` — scheduled job fires, webhook triggers agent, both flow end-to-end.
   - `test/e2e/phase3_pipeline_test.go` — planner → task DAG → implementor → PR → reviewer → merge → unblock dependents.
3. **Staging soak** — a live bot pointed at a test Telegram chat with a scratch GitHub repo. Each phase's `retire` task (1.9, 3.6, 3.7) requires a one-week clean soak before the delete lands.

Feature flags drive staged rollout. Flags are keys in a new `feature_flags` table (or existing config — reuse what's already there). Mapping:

| Flag | Enables | Turned on at |
|---|---|---|
| `mailbox.inbound` | bot → `agent_inbox` path | task 1.6 |
| `mailbox.outbound` | monitor → `agent_outbox` path | task 1.5 |
| `mailbox.dispatcher` | dispatcher actually sends (vs. shadow) | task 1.8 |
| `jobs.scheduler` | scheduler daemon fires for real | task 2.2 |
| `jobs.webhooks` | webhook server accepts real signed requests | task 2.3 |
| `pipeline.task_scheduler` | task scheduler dispatches live | task 3.4 |
| `pipeline.pr_hooks` | PR webhooks reach agents | task 3.5 |

Each task's "turn flag on" step is part of its definition of done; the prior flag must have clean metrics for ≥72 hours first.

---

## Dependency graph (textual, sequential)

```
1.1 → 1.2 → 1.3 → 1.4 → 1.5 → 1.6 → 1.7 → 1.8 → 1.9
                                                  │
                                                  ▼
                           2.1 → 2.2 → 2.3 → 2.4 → 2.5
                                                  │
                                                  ▼
                           3.1 → 3.2 → 3.3 → 3.4 → 3.5 → 3.6 → 3.7
```

Strict sequence. No parallelization inside a phase. Phase boundary requires the prior phase's `retire` task complete. Total: 21 tasks across 3 phases.

---

## Verification of the plan itself

Before starting Phase 1:

1. Confirm no in-flight volta-era work-in-progress touches `internal/queue/`, `internal/monitor/`, or `internal/bot/handlers.go` — if any, coordinate to land it first. Any ongoing work here will rebase-conflict heavily with tasks 1.5, 1.6, 1.9.
2. Confirm `go test ./... && go vet ./...` are green on `main` at plan-start.
3. Confirm Postgres version in staging is ≥ 9.5 (required for `FOR UPDATE SKIP LOCKED`). Current maquinista already depends on it via migration 004's NOTIFY, so this is a formality.
4. Stand up a fresh staging Telegram bot token + group + two topics, and a scratch GitHub repo with `gh` CLI configured on the agent's working dir. These are prerequisites for the integration tests and soak runs in Phases 2 and 3.
