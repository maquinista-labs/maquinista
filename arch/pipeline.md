# Pipeline

The Linear ↔ maquinista bridge (ADR-0005): how MAQ Linear issues become
tasks, and how task state mirrors back onto the Linear board. Concern owner:
`internal/pipeline/`. Agents never call Linear directly — the determinism
boundary is the `tasks` state machine, and the bridge is the only writer of
Linear state.

## Intake (claim loop)

`pipeline.RunBridge` polls Linear every `MAQUINISTA_LINEAR_POLL` (default
60 s, the ADR-0005 intake bound):

- fetches the team's issues in workflow state **Todo** carrying the
  **pipeline** label (env: `LINEAR_API_KEY`, `MAQUINISTA_LINEAR_TEAM_ID`)
- `ClaimIssue` inserts, in ONE transaction:
  - a `tasks` row — status `ready`, project `MAQUINISTA_LINEAR_PROJECT`
    (fallback `MAQUINISTA_PROJECT`), title `[MAQ-n] <title>`, metadata
    `linear_issue_id` + `linear_url`
  - a `linear_issue_map` row — `pending_state` = "In Progress"
- the map row's primary key is the Linear issue UUID, so the INSERT **is**
  the claim: `ON CONFLICT DO NOTHING` + rollback makes re-claims no-ops
- claimed tasks flow through the existing task-scheduler → EnsureAgent →
  agent_inbox path; bridge tasks carry no `metadata.role` yet (EX-02 seeds
  the pipeline-worker soul)

## Mirror (linearSync)

`pipeline.RunSync` reconciles every 10 s. The bookkeeping is diff-based:
`pending_state` is the desired Linear column, `last_synced_state` the last
one successfully pushed; a row is due when they differ and
`next_attempt_at` ≤ now.

Mapping (`DerivedState`), task status → Linear column:

| tasks.status | Linear column |
|---|---|
| `ready`, `claimed` | In Progress |
| `review` | In Review |
| `pending_approval`, `failed` | Needs Human |
| `done` | Done |
| anything else | (skipped) |

- an explicit `pending_state` (written by future pipeline code — e.g.
  "Changes Requested" from a review verdict, EX-03) **wins** over the
  derived value; the derived value fills in only while nothing unsynced is
  pending
- failed pushes retry with exponential backoff: 15 s base, doubling, 10 min
  cap; `attempts`/`next_attempt_at` carry the state across restarts
- diff-based reconcile is self-healing: a missed tick or a Linear outage
  degrades to backoff, never to a lost transition
- a state name missing from the team (operator renamed a column) backs off
  like any other failure instead of hot-looping

## Env contract

| Variable | Meaning | Default |
|---|---|---|
| `LINEAR_API_KEY` | Linear API key (raw, no Bearer) | required to enable |
| `MAQUINISTA_LINEAR_TEAM_ID` | Linear team UUID (MAQ) | required to enable |
| `MAQUINISTA_LINEAR_PROJECT` | project_id stamped on claimed tasks | falls back to `MAQUINISTA_PROJECT` |
| `MAQUINISTA_LINEAR_POLL` | claim-loop interval | `60s` |

Without key + team the bridge is a logged no-op; nothing else in the
orchestrator changes.

## TODO

- worker / reviewer / fixer / merger souls + the PR loop: EX-02+ of
  ADR-0005 (`ADRs/0005-linear-pr-iteration-pipeline.md`)
- `tasks.review_rounds` accounting lands with the review loop (EX-03/EX-04)
