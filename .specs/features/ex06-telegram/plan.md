# EX-06 Telegram plumbing — plan

## Why

The pipeline is fully autonomous up to `merged`, but a human watching only
Telegram sees nothing: verdicts, merge proposals, and needs-human questions
exist as `task_context` rows and log lines. ADR-0005 EX-06: "pipeline topic,
verdict summaries, merge proposals, needs-human questions (relay exists; wire
content)." EX-05 also left four review follow-ups that land here.

## Contracts this builds on (frozen, do not re-litigate)

- ADR-0005 EX-06: pipeline topic, verdict summaries, merge proposals,
  needs-human questions — **relay exists; wire content**.
- Delivery path is frozen: `agent_outbox` → relay (`fanoutDeliveries`) →
  `channel_deliveries` → dispatcher. No new channel, no new dispatcher.
- The dispatcher renders `content->>'text'` (top-level `text` wins over
  `{parts:[]}`) — notification content is `{"type":"text","text":…}`.
- EX-05 review follow-ups (adopted into this plan): enqueue TOCTOU →
  partial unique index; AUTO_MERGE red-PR retry churn → cap; nits —
  `ReleaseMergeEntry` guards on `status='merging'`, `AUTO_MERGE` env
  accepts true/yes spellings.
- **Deferred from EX-05, still deferred:** conflict → merger-agent spawn.
  A merger done-path needs a `MarkDone` branch (its `claimed_by` guard does
  not fit the merge flow) plus a conflict-resolution soul — real design
  work, not 0.5d plumbing. v1 keeps conflict → pending_approval + Telegram
  question; revisit on pilot feedback.

## Design

**Synthetic notifier agent.** Pipeline events are outbox rows written by a
synthetic `agents` row `id='pipeline'` (role `notifier`, status `stopped`,
handle `Pipeline`, session/window `pipeline`). This rides the entire existing
path with zero new delivery code and gives dashboard visibility for free
(dashboard reads outbox directly). The seed row is required because
`agent_outbox.agent_id` and `topic_agent_bindings.agent_id` both FK to
`agents`.

Status/role chosen so every existing daemon that scans `agents` ignores the
row: reconcile selects `role='user'` (no pane respawn), sidecar + monitor
select `status IN ('running','idle','working')` (no PTY, no tailing),
closeOrphanedTopics only reaps `archived|dead` (binding survives), agent-topic
provisioner selects `role='user'` (no double topic).

**Pipeline topic.** The bot's topic provisioner gains `ensurePipelineTopic`:
if the `pipeline` agent row exists, is alive, and has no owner binding,
create forum topic "Pipeline" in the allowed group and insert the owner
binding (`user_id=AllowedUsers[0]`, `chat_id`, `thread_id` + `topic_id` =
thread). The relay binding leg then delivers every `pipeline` outbox row to
the topic; dedup + retries are the stock outbox/delivery machinery.
(Provisioner code needs a live Telegram API — not unit-tested, matching
`provisionMissingTopics`.)

**Notify helper** (`internal/pipeline/notify.go`): `Notify(ctx, pool, text)`
commits one outbox row for the pipeline agent; `notifyf` logs failures and
never fails the caller's transition. Task titles come from a small lookup so
summaries read like a board, not a UUID list.

**Emission points** (each inside the already-guarded transition, exactly
once):

- verdict applied → summary: approve = merge proposal
  ("ready_to_merge — approve with `maquinista approve <id>` or set
  `PIPELINE_AUTO_MERGE=1`"); request_changes = round + fixer note;
  needs_human / round-cap = question.
- watchdog park → question with the stall note.
- gh merge: merged → note (PR #, base, SHA); rebase conflict → question
  with conflict files; CI failed **at cap** → question + entry failed +
  task `pending_approval`; infra `failMerge` → warning (queue won't
  retry, someone must look).
- CI failed below cap and CI pending stay silent — that was EX-05's
  spam bug; the cap fixes it.

**Retry cap.** `merge_queue.attempts INT NOT NULL DEFAULT 0`;
`db.BumpMergeAttempts` increments and returns the count;
`MAQUINISTA_MERGE_ATTEMPTS_MAX` (default 5) moves the cap. On red CI below
cap: release as today (silent). At cap: entry `failed`, task
`pending_approval`, question out.

**TOCTOU index.** Partial unique index on `merge_queue (task_id) WHERE
status IN ('pending','merging')`; `EnqueueMerge` gets `ON CONFLICT DO
NOTHING` so the enqueue pass and the approve verb's demand-enqueue become
idempotent instead of erroring.

**Nits.** `ReleaseMergeEntry … AND status='merging'` (a released entry that
raced to a terminal state must not be resurrected); `AUTO_MERGE` env accepts
`1/true/yes/t/y` case-insensitively.

## Non-goals

- No interactive Telegram buttons / approve callbacks (v1: CLI verb).
- No merger-agent conflict resolution (deferred, see above).
- No per-event dedup table — each emission sits inside a transition guarded
  by an UPDATE … WHERE status guard; the pass loop is the only writer.
