# Pipeline

The ticket-system ↔ maquinista bridge (ADR-0005, ADR-0006): how ticket-system
issues become tasks, and how task state mirrors back onto the board. Concern
owner: `internal/pipeline/`. Agents never call the ticket system directly —
the determinism boundary is the `tasks` state machine, and the sync loop is
the only writer of board state. Provider-neutral by design (ADR-0006): core
speaks `TicketProvider` + canonical `Column`; vendor specifics live in the
provider implementation (`linear.go` for Linear).

## Intake (claim loop)

`pipeline.RunBridge` polls the ticket system every `MAQUINISTA_TICKETS_POLL`
(default 60 s, the ADR-0005 intake bound):

- fetches the team's issues in workflow state **Todo** carrying the
  **pipeline** label — via `TicketProvider.IntakeIssues` (provider chosen by
  `MAQUINISTA_TICKETS_PROVIDER`, default `linear`)
- `ClaimIssue` inserts, in ONE transaction:
  - a `tasks` row — status `ready`, project `MAQUINISTA_TICKETS_PROJECT`
    (fallback `MAQUINISTA_PROJECT`), title `[<Key>] <title>`, metadata
    `ticket_issue_id` + `ticket_url`
  - a `ticket_issue_map` row — `pending_state` = "In Progress"
- the map row's primary key is the provider issue ID, so the INSERT **is**
  the claim: `ON CONFLICT DO NOTHING` + rollback makes re-claims no-ops
- claimed tasks flow through the task-scheduler → EnsureAgent → agent_inbox
  path (wired in `orchestrator start` since EX-07, see "Task scheduler"
  below); bridge tasks carry no `metadata.role` yet (default `implementor`,
  the pipeline-worker soul from migration 035)

## Task scheduler (EX-07)

`taskscheduler.Run` runs inside `orchestrator start` (cmd_start.go), in the
same tickets-enabled block as review dispatch. Wake triggers: LISTEN
`task_events` + 30 s poll fallback. `DispatchOne` claims one `ready` task
with no live agent (`FOR UPDATE SKIP LOCKED`, `uq_agents_task_live` keeps
replicas honest), flips it to `claimed`, then the cmd-side
`ensureTaskWorker` adapter spawns the implementor:

- worktree guard: the task must have a usable `worktree_path` — SpawnFresh
  does not stat; a bad path fails the spawn with a readable error and
  DispatchOne reverts the task to `ready` for the next tick
- id mint: `<role>-<taskID>[-rN]` via `pipeline.MintWorkerID` (same shape as
  the reviewer mint; role default `implementor`, overridable via
  `tasks.metadata->>'role'`)
- exec contract: `pipeline.ResolveWorkerExec` reads the pipeline-worker
  soul's frozen extras (`default_runner` + `reasoning_class`); on read
  failure the spawn falls back to `cfg.DefaultRunner` (workers are cheap and
  replaceable — review dispatch treats the same failure as hard)
- everything else is `agentspawn.SpawnFresh`: soul clone (pipeline-worker),
  memory seed, tmux window, sidecar inbox goroutine
- the scheduler then enqueues `/work-on-task <id>` (external_msg_id
  `task:<id>` dedup) and sets `tasks.claimed_by`; `HealMissingInbox` covers
  the crash-between-claim-and-enqueue wedge

The standalone `maquinista task-scheduler` subcommand keeps the
orchestrator.EnsureAgent stub (row-only, no pty) for debugging alongside a
running bot.

## Mirror (sync)

`pipeline.RunSync` reconciles every 10 s. The bookkeeping is diff-based:
`pending_state` is the desired board column, `last_synced_state` the last
one successfully pushed; a row is due when they differ and
`next_attempt_at` ≤ now.

Mapping (`DerivedState`), task status → canonical column:

| tasks.status | Column |
|---|---|
| `ready`, `claimed` | In Progress |
| `review` | In Review |
| `changes_requested` | Changes Requested |
| `ready_to_merge` | Ready to Merge |
| `pending_approval`, `failed` | Needs Human |
| `done` | Done |
| anything else | (skipped) |

Canonical names are the stored values; the provider maps them to its own
column ids via `TicketProvider.Columns` before pushing (`SetIssueColumn`).

- the board mirror is **purely derived** from `tasks.status` — dispatch
  (below) never writes board state, it only advances task statuses and
  lets the sync loop mirror them. The explicit `pending_state` override
  mechanism remains as an escape hatch for future manual interventions;
  nothing in the automated loop writes it
- failed pushes retry with exponential backoff: 15 s base, doubling, 10 min
  cap; `attempts`/`next_attempt_at` carry the state across restarts
- diff-based reconcile is self-healing: a missed tick or an outage
  degrades to backoff, never to a lost transition
- a state name missing from the team (operator renamed a column) backs off
  like any other failure instead of hot-looping
- **PR-link sync (MAQ-10)** — the same tick also runs `SyncIssueLinks`:
  every mapped task whose `pr_url` is set and differs from
  `ticket_issue_map.pr_url_synced` (the last URL successfully written,
  migration 037) gets one `TicketProvider.AddIssueLink` write — for Linear,
  a comment carrying the raw URL. `pr_url_synced` is stamped only after a
  successful push, so ticks are idempotent (exactly one write per URL) and
  a failed push retries on the next tick. Tasks without a PR select
  nothing — no empty-link writes.

## Review dispatch (EX-03)

`pipeline.RunDispatch` (in `internal/pipeline/dispatch.go`) runs four passes
per tick (default 10 s, same cadence as sync). It never talks to the ticket
system — everything below is `tasks`/`agents` bookkeeping, and the board
sees the results only through the sync mirror.

**Entry.** `db.MarkDone` branches: a pipeline task (metadata
`ticket_issue_id`) marked done by its worker goes to `review`, not `done` —
review is part of the done path, not an optional extra. Plain tasks are
unaffected.

**Spawn pass.** For every pipeline task in `review` with a worktree and no
live reviewer agent:

- mints a fresh id `reviewer-<task>[-rN]` (never reuses an id)
- **zero-author guard**: the task's author is the agent that recorded the
  latest `task_context` result; a minted id equal to that author is
  pathological — the task parks in `pending_approval` instead of spawning
  (structural: fresh mints can't collide; the explicit check catches
  identity corruption)
- resolves runner + model from the `pipeline-reviewer` template's frozen
  extras (`default_runner`, `reasoning_class`) — `ResolveExec`: class
  `high` → `MAQUINISTA_PI_MODEL_HIGH`, else `MAQUINISTA_PI_MODEL`, empty
  model = the runner's own chain
- spawns via `ReviewSpawner` (wraps `agentspawn.SpawnFresh`: agents row
  task-bound, soul clone, tmux pane, sidecar), then bumps
  `tasks.review_rounds` and enqueues the round prompt in ONE tx
  (`external_msg_id = review:<task>:<round>` dedups)
- on a `uq_agents_task_live` spawn failure, runs the **stuck-implementor
  self-heal (MAQ-14)**: if the blocking live row is the task's implementor
  whose last outbox activity is older than
  `MAQUINISTA_IMPLEMENTOR_IDLE_AFTER` (default 10m), it is auto-retired
  (`status='dead'`, guarded UPDATE — fires once, which is also the
  exactly-once Pipeline-topic notification dedup) and the reviewer spawns
  next tick. Fresh implementors are left alone (silent retry); the same
  heal guards the fixer pass.

**Prompt heal.** A crash between spawn and enqueue leaves a live reviewer
with no prompt; the heal pass re-enqueues exactly one (dedup'd) on the next
tick.

**Verdict pass.** Scans each live reviewer's newest outbox rows for the
contract verdict line (`ParseVerdict`, line-anchored, exact three-value
vocabulary). The first well-formed line wins; a malformed `VERDICT:`-ish
line is logged loudly and never transitions. On a verdict, one tx:

- task transition: `approve` → `ready_to_merge`,
  `request_changes` → `changes_requested`, `needs_human` →
  `pending_approval` (guarded on the task still being in `review` — a raced
  row is left untouched). **Round cap (EX-04):** the transition is decided
  atomically inside the guarded UPDATE — a `request_changes` landing when
  `review_rounds` has already reached
  `MAQUINISTA_REVIEW_ROUNDS_MAX` (default 3) parks the task in
  `pending_approval` instead, with the verdict row noting the cap.
- verdict recorded in `task_context` (kind `verdict`)
- reviewer retired (`agents.status = 'dead'`) — frees the one-live-slot
  per task (`uq_agents_task_live`) for the next round's mint — and its
  tmux window is killed (best-effort)

**Fixer pass (EX-04).** A pipeline task parked in `changes_requested` with a
worktree and a `request_changes` verdict row is re-claimed by a fresh fixer
session in the SAME worktree/PR:

- mints `fixer-<task>[-rN]` (role `fixer`), resolves exec from the
  `pipeline-fixer` template's frozen extras (class `standard` →
  `MAQUINISTA_PI_MODEL`), spawns via the same `ReviewSpawner`
- the episode is keyed by `review_rounds` (frozen while parked — it only
  bumps at the next reviewer spawn); a `task_context` fix row
  (content `round <N>`) commits FIRST and stops re-spawning for the episode
- the fix prompt (`external_msg_id = fix:<task>:<round>` dedup) embeds the
  reviewer's newest message tail (≤6000 chars — the soul contract puts the
  numbered findings at the top of the final reply); a prompt miss heals on
  the next tick (same shape as the reviewer prompt heal)
- **no zero-author guard by design** — a fixer continuing the previous
  fixer's work is the point; the round N+1 reviewer is always a fresh mint
- **loop closure needs no new transition code**: the fixer ends with
  `maquinista-done` → `db.MarkDone` done-path branch → `review` → the
  reviewer spawn pass mints a fresh reviewer and bumps the round

**Watchdog.** A live reviewer (in `review`) or fixer (in
`changes_requested`) with NO activity signal for longer than
`MAQUINISTA_REVIEW_TIMEOUT` (default 2h) parks the task in
`pending_approval` with a watchdog verdict row and retires the pane.
Activity is two-channel (MAQ-9): `agent_outbox` rows (assistant text the
monitor streams) OR transcript growth (`agents.last_transcript_at`, written
throttled by the monitor on JSONL offset advance — a healthy agent
mid-command emits tool events but no outbox text). Parking requires BOTH
channels silent for the whole window; agents younger than the timeout are
exempt entirely (a newborn has no signal on either channel yet — parking on
sight murders slow-booting spawns). The malformed-verdict case is
deliberately left to the watchdog: the parser never guesses, the timeout is
the backstop — and a stalled fixer is bounded the same way.

## Env contract

| Variable | Meaning | Default |
|---|---|---|
| `MAQUINISTA_TICKETS_PROVIDER` | provider name for `pipeline.NewProvider` | `linear` |
| `MAQUINISTA_TICKETS_API_KEY` | ticket-system API key | required to enable |
| `MAQUINISTA_TICKETS_TEAM_ID` | team/board id intake polls | required to enable |
| `MAQUINISTA_TICKETS_PROJECT` | project_id stamped on claimed tasks | falls back to `MAQUINISTA_PROJECT` |
| `MAQUINISTA_TICKETS_POLL` | claim-loop interval | `60s` |
| `MAQUINISTA_REVIEW_TIMEOUT` | dispatch watchdog stall bound (reviewers + fixers) | `2h` |
| `MAQUINISTA_IMPLEMENTOR_IDLE_AFTER` | stuck-implementor self-heal bound: outbox idleness past this retires a `uq_agents_task_live`-blocking implementor row | `10m` |
| `MAQUINISTA_REVIEW_ROUNDS_MAX` | fixer-loop cap: the request_changes landing at/after this review round parks the task | `3` |
| `MAQUINISTA_PI_MODEL_HIGH` | model for `reasoning_class: high` reviewers | falls back to `MAQUINISTA_PI_MODEL` |
| `PIPELINE_MERGE_MODE` | merge driver: `local` (MergeNoFF in repo) or `gh` (remote PR flow) | `local` |
| `PIPELINE_AUTO_MERGE` | gh mode only: truthy (`1`/`true`/`yes`/`t`/`y`) lets the queue merge without the approve verb | `0` |
| `MAQUINISTA_MERGE_ATTEMPTS_MAX` | red-PR reclaim cap before the task parks needs-human | `5` |

Without key + team the bridge is a logged no-op; nothing else in the
orchestrator changes. The Linear provider additionally honors the legacy
`LINEAR_API_KEY` as a key fallback (migration sugar; core never reads it).
Core neutrality is enforced by check: under `internal/pipeline/` only
`linear.go` + `linear_test.go` may mention linear; under `internal/db/` and
`cmd/` none may.

### Merge mode: gh (EX-05)

`PIPELINE_MERGE_MODE=gh` extends the determinism boundary to merging. The
driver is still the merge_queue entry — one row per merge attempt, same
statuses (`pending → merging → merged|conflict|failed`) as the local flow:

- **Enqueue pass** — each dispatch tick, tasks landing in `ready_to_merge`
  with a PR URL and a worktree get an entry (branch derived from the
  worktree's HEAD, base from `origin/HEAD`).
- **Processing** — `maquinista merge` (or the approve verb, below) claims an
  entry and drives the remote: `fetch` → `rebase origin/<base>` →
  `push --force-with-lease` → CI gate (`gh pr view statusCheckRollup`) →
  `gh pr merge --squash`. After the squash lands: entry `merged` with the
  squash SHA, task `ready_to_merge → done` with `pr_state=merged`, one
  observation, a best-effort board push to Done, then worktree + local +
  remote branch cleanup.
- **CI gate** — pending checks release the entry back to `pending` (a later
  pass retries); failed checks bump `attempts` and release silently below
  the cap, at the cap (default 5, `MAQUINISTA_MERGE_ATTEMPTS_MAX`) the entry
  fails, the task parks `pending_approval`, and the Pipeline topic gets the
  question (EX-06). No checks configured counts as green.
- **Conflicts** — the rebase is aborted and the task parks at
  `pending_approval` (Needs Human on the board) with the conflicting files
  in the observation and in the Pipeline-topic note; conflicts are never
  auto-resolved.
- **Human gate** — `PIPELINE_AUTO_MERGE=0` (default) makes every processing
  pass release the entry untouched; `maquinista approve <task>` on a
  `ready_to_merge` task runs the full flow immediately, overriding the gate
  for that one merge.
- **Telegram plumbing (EX-06)** — merge lifecycle notes (merged, conflict,
  infra failure, CI-cap) ride the stock delivery path via the synthetic
  `pipeline` notifier agent (migration `036`): `Notify` opens a tx, appends
  one `agent_outbox` row for the agent, commits — the relay's binding leg
  fans it into `channel_deliveries` for the Pipeline topic provisioned by
  the bot (`ensurePipelineTopic`). Failures are logged, never escalated.
  Every task mention that has a `pr_url` carries the link — verdict
  summaries (`notifyVerdict`), watchdog parks, and all merge-flow notes —
  via `prLinkSuffix`; tasks without a PR keep the old linkless text
  (MAQ-10: no null/empty links).
- GitHub is behind `pipeline.GhRunner` (interface: `PRChecks` +
  `PRMergeSquash`); production uses the gh CLI (`internal/gh`).

## Role souls

EX-02 seeds five pipeline role templates (migration
`035_seed_pipeline_souls.sql`, `028` style — catalog entries only; the
dispatcher clones them per spawn):

| Template | Role | reasoning_class |
|---|---|---|
| `pipeline-worker` | spec-first task execution (tlc-spec-lean: PLAN/CHECKS/BUILD/VERIFY, validators, `maquinista-done`) | `standard` |
| `pipeline-reviewer` | independent diff review — fresh per round, zero-author | `high` |
| `pipeline-arbiter` | adjudication of contested / repeated `request_changes` | `high` |
| `pipeline-fixer` | reviewer-findings resolution in the same worktree/PR | `standard` |
| `pipeline-merger` | rebase + `gh pr checks` gate + propose/merge | `standard` |

Cross-exercise contracts frozen here (EX-03 dispatch + verdict parsing build
on these literals):

- **Verdict line** — reviewer/arbiter sessions end their output with exactly
  one line: `VERDICT: approve` / `VERDICT: request_changes` /
  `VERDICT: needs_human`.
- **Dispatch hints** — every pipeline template carries `extras` keys
  `default_runner` (all `pi`) and `reasoning_class`; EX-03 resolves them to
  runner + model at dispatch.

Souls stay runner-agnostic (ADR-0005 revisit triggers): re-binding a role to
a new harness is an extras edit, not a soul rewrite.

## TODO

- fixer re-claim loop: **shipped (EX-04)** — see "Fixer pass" above; round
  cap + fixer watchdog included
- merge mode: **shipped (EX-05)** — see "Merge mode: gh" above; enqueue
  pass, rebase + CI gate, approve-verb override included
- Telegram plumbing: EX-06
- worker spawn wiring: **shipped (EX-07)** — see "Task scheduler" above;
  task-scheduler in `orchestrator start`, ensureTaskWorker → SpawnFresh

