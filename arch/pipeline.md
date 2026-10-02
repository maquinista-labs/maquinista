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
  agent_inbox path; bridge tasks carry no `metadata.role` yet (EX-02 seeds
  the pipeline-worker soul)

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
| `pending_approval`, `failed` | Needs Human |
| `done` | Done |
| anything else | (skipped) |

Canonical names are the stored values; the provider maps them to its own
column ids via `TicketProvider.Columns` before pushing (`SetIssueColumn`).

- an explicit `pending_state` (written by future pipeline code — e.g.
  "Changes Requested" from a review verdict, EX-03) **wins** over the
  derived value; the derived value fills in only while nothing unsynced is
  pending
- failed pushes retry with exponential backoff: 15 s base, doubling, 10 min
  cap; `attempts`/`next_attempt_at` carry the state across restarts
- diff-based reconcile is self-healing: a missed tick or an outage
  degrades to backoff, never to a lost transition
- a state name missing from the team (operator renamed a column) backs off
  like any other failure instead of hot-looping

## Env contract

| Variable | Meaning | Default |
|---|---|---|
| `MAQUINISTA_TICKETS_PROVIDER` | provider name for `pipeline.NewProvider` | `linear` |
| `MAQUINISTA_TICKETS_API_KEY` | ticket-system API key | required to enable |
| `MAQUINISTA_TICKETS_TEAM_ID` | team/board id intake polls | required to enable |
| `MAQUINISTA_TICKETS_PROJECT` | project_id stamped on claimed tasks | falls back to `MAQUINISTA_PROJECT` |
| `MAQUINISTA_TICKETS_POLL` | claim-loop interval | `60s` |

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

- dispatch + verdict parsing + zero-author check + review rounds: EX-03 of
  ADR-0005 (`ADRs/0005-linear-pr-iteration-pipeline.md`)
- fixer re-claim loop: EX-04; merge mode: EX-05; Telegram plumbing: EX-06
- `tasks.review_rounds` accounting lands with the review loop (EX-03/EX-04)

