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
- claimed tasks flow through the existing task-scheduler → EnsureAgent →
  agent_inbox path; bridge tasks carry no `metadata.role` yet (the
  pipeline-worker soul is seeded by migration 035 for future use)

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
`changes_requested`) with NO outbox activity (the monitor writes rows as the
agent streams) for longer than `MAQUINISTA_REVIEW_TIMEOUT` (default 2h)
parks the task in `pending_approval` with a watchdog verdict row and retires
the pane. The malformed-verdict case is deliberately left to the watchdog:
the parser never guesses, the timeout is the backstop — and a stalled fixer
is bounded the same way.

## Env contract

| Variable | Meaning | Default |
|---|---|---|
| `MAQUINISTA_TICKETS_PROVIDER` | provider name for `pipeline.NewProvider` | `linear` |
| `MAQUINISTA_TICKETS_API_KEY` | ticket-system API key | required to enable |
| `MAQUINISTA_TICKETS_TEAM_ID` | team/board id intake polls | required to enable |
| `MAQUINISTA_TICKETS_PROJECT` | project_id stamped on claimed tasks | falls back to `MAQUINISTA_PROJECT` |
| `MAQUINISTA_TICKETS_POLL` | claim-loop interval | `60s` |
| `MAQUINISTA_REVIEW_TIMEOUT` | dispatch watchdog stall bound (reviewers + fixers) | `2h` |
| `MAQUINISTA_REVIEW_ROUNDS_MAX` | fixer-loop cap: the request_changes landing at/after this review round parks the task | `3` |
| `MAQUINISTA_PI_MODEL_HIGH` | model for `reasoning_class: high` reviewers | falls back to `MAQUINISTA_PI_MODEL` |

Without key + team the bridge is a logged no-op; nothing else in the
orchestrator changes. The Linear provider additionally honors the legacy
`LINEAR_API_KEY` as a key fallback (migration sugar; core never reads it).
Core neutrality is enforced by check: under `internal/pipeline/` only
`linear.go` + `linear_test.go` may mention linear; under `internal/db/` and
`cmd/` none may.

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
- merge mode: EX-05; Telegram plumbing: EX-06
- the merger (EX-05) consumes `ready_to_merge` → done/merged

