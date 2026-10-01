# Linear bridge (linear-bridge)

Sources:

- `ADRs/0005-linear-pr-iteration-pipeline.md:64-93` - **binding for the obligations**: EX-01 = poller, claim, `linearSync` mirror, migrations (obligations 1, 4, 11)
- `ADRs/0005-linear-pr-iteration-pipeline.md:135-139` - bridge shape: task-row-first claim, mirror goroutine with retry, agents never call Linear
- `internal/taskscheduler/taskscheduler.go:95-105` - the ready-claim query (`FOR UPDATE SKIP LOCKED`) the bridge must satisfy for its tasks to be picked up
- `internal/orchestrator/orchestrator.go:115-119` - direct dispatch retired; task flow goes task-scheduler → EnsureAgent → agent_inbox
- `internal/db/queries.go:261-271` - AtomicClaim parallel shape (ready + project filter) confirming status conventions
- `internal/db/migrations/011_task_pipeline.sql:25-27` - tasks.status state machine comment
- `internal/db/migration_009_test.go` + `internal/dbtest` (PgContainer) - house precedent for DB-backed migration proofs
- `.specs/features/pi-integration/plan.md` - house spec format

Review mode: MAQ-2 carries no `plan-gate` label, so per ADR-0005 ("Spec visibility") the tlc
human stop moves to PR review — this plan is reviewed as part of the PR, not before the build.

## Problem

ADR-0005's pipeline starts with intake: a Linear issue entering the MAQ team's Todo column
(label `pipeline`) must become a maquinista task within 60 s, and every later Postgres state
transition must mirror back to the Linear board, with retry when Linear is unreachable. Today
neither direction exists: nothing watches Linear, and no component translates `tasks.status`
changes into Linear column moves. Workers (EX-02+) and reviewers (EX-03+) depend on both
halves; without the bridge the rest of the pipeline has no feed and no board reflection.

When this ships: dropping a `pipeline`-labeled issue into MAQ Todo produces a `ready` task the
existing task-scheduler can claim, and moving the task through `review`/`done` moves the
Linear card correspondingly.

## Out of scope

| Excluded | Why |
| --- | --- |
| Worker/reviewer/fixer/merger souls + dispatch | EX-02/EX-03/EX-04; this task only feeds the queue |
| Changes Requested / Ready to Merge transitions | Producers arrive in EX-03/EX-05; the sync design accepts explicit states but adds none |
| PR creation, branch push, `gh` wiring | EX-03+; task rows here have no pr_url yet |
| Linear webhooks | Box has no public endpoint (ADR-0005 Context); polling only |
| Linear state creation/renaming on the board | One-time operator setup; the bridge consumes names as-is |

## Assumptions

| Assumption | Chosen default | Rationale | Confirmed? |
| --- | --- | --- | --- |
| Poll matches by workflow state name "Todo", not UUID | name-based | MAQ workflow states are team-created; renames are deliberate operator acts; names are the readable contract | y |
| `failed` maps to "Needs Human" | mapping per AC 13 | ADR-0005 is silent on failed; human attention beats a silent Done | n - raised at PR review |
| Intervals: claim loop 60 s (ADR-0005 obligation 1), sync loop 10 s | as stated | 60 s is the ADR intake bound; the mirror is faster by design because retries ride it | y |
| Workflow-state ids are re-fetched each reconcile tick | no persistent cache | ≈7 req/min steady state is far under Linear's rate limits; avoids invalidation bugs | y |
| Bridge tasks carry no `metadata.role` in v1 | default "implementor" fallback (`taskscheduler.go:105-111`) | an unknown soul name would flow into EnsureAgent before EX-02 seeds it; EX-02 adds the role when the soul exists | y |

**Open questions:**

| # | Kind | Question | Until answered |
| --- | --- | --- | --- |
| 1 | blocks go-live | Live end-to-end pilot: real MAQ issue claim → scheduler spawn → board mirror (EX-07 scope) | Code may merge; box enablement waits for souls (EX-02) + deploy (EX-07) |

## Criteria

Grouped by slice - one observable outcome each, never a layer. Numbering runs
across the whole plan.

### S1: pipeline schema migration (P1)

**Acceptance Criteria**

1. WHEN RunMigrations applies on a clean database THEN the schema SHALL contain table linear_issue_map keyed by
   linear_issue_id TEXT PRIMARY KEY, with identifier TEXT NOT NULL, team_id TEXT NOT NULL, task_id TEXT NOT NULL
   UNIQUE REFERENCES tasks(id) ON DELETE CASCADE, last_synced_state TEXT, pending_state TEXT, attempts INTEGER
   NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), synced_at TIMESTAMPTZ, created_at and
   updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
2. WHEN RunMigrations applies THEN tasks SHALL gain column review_rounds INTEGER NOT NULL DEFAULT 0
3. IF a tasks row is deleted THEN its linear_issue_map row SHALL be removed by cascade

**Independent test:** `go test ./internal/db/ -run TestMigration034 -v` (dbtest.PgContainer + RunMigrations + information_schema/columns asserts), following `internal/db/migration_009_test.go`.

### S2: Linear GraphQL client (P1)

**Acceptance Criteria**

4. WHEN LinearClient executes any document THEN the request SHALL carry header "Authorization" set to the raw API key (no Bearer prefix) with Content-Type application/json
5. WHEN the response body carries a GraphQL errors array THEN the client SHALL return an error whose text contains the first message
6. IF the API answers with a non-200 HTTP status THEN the client SHALL return an error containing that status code
7. WHEN FetchTodoIssues queries the MAQ board THEN the document SHALL filter issues by team id, workflow state name "Todo" and label name "pipeline", selecting id, identifier, title, description, url
8. WHEN SetIssueState completes a 200 state update THEN the client SHALL unwrap the data envelope and return the issue's resulting state name

**Independent test:** `go test ./internal/pipeline/ -run TestLinearClient -v` (httptest stub recording headers/documents/responses).

### S3: claim — Linear issue → task row (P1)

**Acceptance Criteria**

9. WHEN ClaimIssue runs for an unmapped issue THEN the same transaction SHALL insert a tasks row with status "ready", project_id from config, title prefixed "[<identifier>] " and metadata fields linear_issue_id and linear_url
10. WHEN ClaimIssue runs for an unmapped issue THEN the same transaction SHALL insert the linear_issue_map row with pending_state "In Progress", last_synced_state NULL and attempts 0
11. WHEN ClaimIssue runs for an already-mapped issue THEN it SHALL return created=false leaving exactly one tasks row and an unchanged map row
12. WHEN ClaimIssue commits THEN the task-scheduler ready-claim query (`taskscheduler.go:97`) SHALL match the new task

**Independent test:** `go test ./internal/pipeline/ -run TestClaim -v` (dbtest.PgContainer; AC 12 runs the literal scheduler SELECT).

### S4: linearSync — Postgres transitions → Linear board (P1)

**Acceptance Criteria**

13. WHEN DerivedState maps task statuses THEN the result SHALL be ready and claimed to "In Progress", review to "In Review", pending_approval and failed to "Needs Human", done to "Done", any other status to empty
14. WHEN a map row holds pending_state distinct from last_synced_state and next_attempt_at is due THEN the syncer SHALL push that state via SetIssueState and set last_synced_state, attempts 0 and synced_at NOW()
15. WHEN the push errors THEN the syncer SHALL increment attempts and set next_attempt_at to NOW() plus exponential backoff (15 s base doubling, 10 min cap) keeping last_synced_state unchanged
16. WHEN a mapped task's status changed after its last sync THEN reconcile SHALL set pending_state to the derived state only while the previous pending_state is already synced
17. IF a mapped task carries a status with no mapping THEN the syncer SHALL skip the row leaving it unpushed

**Independent test:** `go test ./internal/pipeline/ -run TestSync -v` (dbtest.PgContainer + fake Linear client).

### S5: wiring + docs (P2)

**Acceptance Criteria**

18. WHEN BridgeConfig.FromEnv runs without LINEAR_API_KEY or MAQUINISTA_LINEAR_TEAM_ID THEN Enabled SHALL be false
19. WHEN both variables are set THEN Enabled SHALL be true with PollInterval 60 s, overridable by MAQUINISTA_LINEAR_POLL
20. WHEN cmd_start enables the bridge THEN it SHALL start the claim and sync loops in goroutines, logging "pipeline: linear bridge started" once
21. WHEN the feature lands THEN arch/pipeline.md SHALL document the claim loop, the sync loop and the status mapping table
22. WHEN arch/README.md lists concern files THEN it SHALL include the pipeline entry pointing at arch/pipeline.md

**Independent test:** `go test ./internal/pipeline/ -run TestBridgeConfig -v` plus `rg -n 'pipeline: linear bridge started' cmd/maquinista/cmd_start.go` and `rg -n 'pipeline' arch/README.md`.

## Traceability

| ID | Slice | Criteria | Status |
| --- | --- | --- | --- |
| LB-01 | S1 | 1–3 | Pending |
| LB-02 | S2 | 4–8 | Pending |
| LB-03 | S3 | 9–12 | Pending |
| LB-04 | S4 | 13–17 | Pending |
| LB-05 | S5 | 18–22 | Pending |

## Observable

Every item of every surface this feature exposes. `n/a` needs its reason.

| Surface | Decision | Landing |
| --- | --- | --- |
| Linear board: Todo → In Progress after claim | claim writes pending_state "In Progress"; sync pushes it | AC 10, AC 14 |
| Linear board mirror of task transitions | derived mapping moves the card per tasks.status | AC 13, AC 14, AC 16 |
| Linear API outage | backoff retries; diff-based reconcile loses no transition | AC 15 |
| Task-scheduler pickup of bridge tasks | row claimable as "ready" on commit | AC 12 |
| Box/dev without Linear env vars | bridge disabled, logged no-op | AC 18 |
| Bot commands / payloads | n/a - no new bot surface; existing flows untouched |
| Dashboard | n/a - dashboard reads tasks/agents generically, no new UI |
| CLI | n/a - no new maquinista subcommand |

## Flow

Reuses instead of duplicating: the task-scheduler claim, EnsureAgent spawn and
agent_inbox dispatch are all existing; the bridge only feeds rows and mirrors state.

1. Linear MAQ board Todo column (exists) -> `internal/pipeline/bridge.go` (new) 60 s tick via FetchTodoIssues
2. -> ClaimIssue (new): one transaction inserting tasks (status "ready") + linear_issue_map via `internal/db/migrations/034_linear_bridge.sql` (new)
3. -> `internal/taskscheduler` (exists) claims the ready task -> `orchestrator.EnsureAgent` (exists) -> `agent_inbox` (exists) /work-on-task — worker souls are EX-02, out of scope
4. -> `internal/pipeline/sync.go` (new) 10 s tick: JOIN linear_issue_map↔tasks -> DerivedState -> `internal/pipeline/linear.go` (new) SetIssueState -> Linear board (exists)
5. `cmd/maquinista/cmd_start.go` (exists) starts both loops when enabled; `arch/pipeline.md` (new) documents the concern

## Relations

- linear_issue_map is 1:1 with tasks (UNIQUE task_id, FK ON DELETE CASCADE); its primary
  key is the Linear issue UUID, making the INSERT the single-owner claim.
- No other entity changes; tasks gains only review_rounds (unused until EX-03/EX-04).
- merge_queue is untouched in this task.

## Surface

None - nothing consumed outside: the feature is two internal goroutines plus a table. The
operator-facing env contract is recorded as a door below; no route, API or payload is
introduced.

## Landing

| One-way door | Literal shape | Alternative rejected |
| --- | --- | --- |
| linear_issue_map DDL | `CREATE TABLE linear_issue_map (linear_issue_id TEXT PRIMARY KEY, ... task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE, ...)` in migration 034 | Nullable columns on tasks - claim uniqueness and sync bookkeeping would pollute tasks and lose FK/UNIQUE enforcement, which are the claim primitive |
| Claim is INSERT-first | `INSERT ... ON CONFLICT (linear_issue_id) DO NOTHING` inside the task-insert transaction; created = row-returned | SELECT-then-INSERT - TOCTOU double claim across concurrent ticks/instances |
| Bridge tasks insert as "ready" | `INSERT INTO tasks (..., status, ...) VALUES (..., 'ready', ...)` | status 'pending' - dispatch is retired (`orchestrator.go:115-119`) and `taskscheduler.go:97` claims 'ready' only, so pending rows strand; corrects the ADR-0005 diagram's "tasks row (pending)" against live code |
| Status→state mapping lives in Go | `DerivedState` switch in `internal/pipeline/sync.go` | SQL CASE - untestable outside a container and duplicated again when EX-03 adds Changes Requested |
| Env namespace | `LINEAR_API_KEY` (Linear's documented credential name) + `MAQUINISTA_LINEAR_TEAM_ID` / `MAQUINISTA_LINEAR_PROJECT` / `MAQUINISTA_LINEAR_POLL` | Unprefixed `LINEAR_TEAM_ID`/`LINEAR_POLL` - collides with other Linear tooling on the operator's boxes (playa scripts already read `LINEAR_API_KEY`) |
| No metadata role on bridge tasks in v1 | metadata holds only linear_issue_id + linear_url | Setting role "pipeline-worker" now - an unknown soul name would reach EnsureAgent before EX-02 seeds it; the default "implementor" fallback is the safe spawn |

Nothing else in this change is hard to reverse: package files are additive, the
goroutines start behind an env gate, and review_rounds is an unused-by-default column.

## Impact

| Front | What changes |
| --- | --- |
| domain | new terms: linear-bridge (intake poller), linearSync (PG→Linear mirror), pipeline task (task with a linear_issue_map row) |
| stored data | new table linear_issue_map; tasks.review_rounds column (default 0, no backfill) |
| existing behavior | none changed - two new goroutines behind an env gate; scheduler, merge queue, relay untouched |
| ops | barceloneta needs LINEAR_API_KEY + MAQUINISTA_LINEAR_TEAM_ID to enable; absent = one logged no-op line at startup |
