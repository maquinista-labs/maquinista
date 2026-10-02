# Linear bridge checks

Profile: light
Plan: `.specs/features/linear-bridge/plan.md`

22 checks in 5 slices · 6 one-way doors · 1 open question, of which 1 blocks go-live
(live pilot, EX-07 scope - not a coverage member)

## Checks

### S1 - pipeline schema migration · 2 files · ~2 KB · ~0.5k

**C1** - `linear_issue_map` exists with the full column set of AC 1: linear_issue_id TEXT PK, identifier TEXT NOT NULL, team_id TEXT NOT NULL, task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE, last_synced_state TEXT, pending_state TEXT, attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), synced_at TIMESTAMPTZ, created_at/updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW() (LB-01, AC 1)
Proof: `go test ./internal/db/ -run TestMigration034_LinearIssueMap -v`

**C2** - `tasks.review_rounds` INTEGER NOT NULL DEFAULT 0 (LB-01, AC 2)
Proof: `go test ./internal/db/ -run TestMigration034_ReviewRounds -v`

**C3** - Deleting a tasks row removes its linear_issue_map row (cascade) (LB-01, AC 3)
Proof: `go test ./internal/db/ -run TestMigration034_Cascade -v`

### S2 - Linear GraphQL client · 2 files · ~4 KB · ~1k

**C4** - Every client request carries `Authorization: <raw key>` (no Bearer prefix) and `Content-Type: application/json` (LB-02, AC 4)
Proof: `go test ./internal/pipeline/ -run TestLinearClient_AuthHeaders -v`

**C5** - A GraphQL `errors` array surfaces as an error whose text contains the first message (LB-02, AC 5)
Proof: `go test ./internal/pipeline/ -run TestLinearClient_GQLErrors -v`

**C6** - A non-200 HTTP answer surfaces as an error containing the status code (LB-02, AC 6)
Proof: `go test ./internal/pipeline/ -run TestLinearClient_HTTPStatus -v`

**C7** - FetchTodoIssues filters by team id, workflow state name "Todo" and label name "pipeline", selecting id, identifier, title, description, url (LB-02, AC 7)
Proof: `go test ./internal/pipeline/ -run TestLinearClient_FetchTodoDocument -v`

**C8** - SetIssueState unwraps the data envelope and returns the issue's resulting state name (LB-02, AC 8)
Proof: `go test ./internal/pipeline/ -run TestLinearClient_SetIssueState -v`

### S3 - claim: Linear issue → task row · 3 files · ~5 KB · ~1.2k

**C9** - ClaimIssue inserts a tasks row with status "ready", configured project_id, "[<identifier>] " title prefix and metadata linear_issue_id + linear_url (LB-03, AC 9)
Proof: `go test ./internal/pipeline/ -run TestClaim_InsertsTaskRow -v`

**C10** - The same transaction inserts the linear_issue_map row with pending_state "In Progress", last_synced_state NULL, attempts 0 (LB-03, AC 10)
Proof: `go test ./internal/pipeline/ -run TestClaim_InsertsMapRow -v`

**C11** - A second ClaimIssue for the same issue returns created=false, leaves exactly one tasks row and an unchanged map row (LB-03, AC 11)
Proof: `go test ./internal/pipeline/ -run TestClaim_Idempotent -v`

**C12** - After commit, the literal task-scheduler ready-claim SELECT (`taskscheduler.go:95-105` shape) matches the new task (LB-03, AC 12)
Proof: `go test ./internal/pipeline/ -run TestClaim_SchedulerQueryMatches -v`

### S4 - linearSync: PG transitions → Linear board · 2 files · ~5 KB · ~1.2k

**C13** - DerivedState: ready|claimed → "In Progress", review → "In Review", pending_approval|failed → "Needs Human", done → "Done", pending (unmapped) → "" (LB-04, AC 13)
Proof: `go test ./internal/pipeline/ -run TestSync_DerivedState -v`

**C14** - A due row with pending_state ≠ last_synced_state is pushed; last_synced_state set, attempts 0, synced_at NOW() (LB-04, AC 14)
Proof: `go test ./internal/pipeline/ -run TestSync_PushesDueRow -v`

**C15** - A failed push increments attempts, moves next_attempt_at out by exponential backoff (15 s base doubling, 10 min cap), keeps last_synced_state (LB-04, AC 15)
Proof: `go test ./internal/pipeline/ -run TestSync_BackoffOnFailure -v`

**C16** - Derived overwrite applies only while the previous pending_state is already synced; an unsynced explicit pending_state survives reconcile (LB-04, AC 16)
Proof: `go test ./internal/pipeline/ -run TestSync_DerivedOverwriteRule -v`

**C17** - A mapped task with a status outside the mapping is skipped, row unpushed (LB-04, AC 17)
Proof: `go test ./internal/pipeline/ -run TestSync_SkipsUnmappedStatus -v`

### S5 - wiring + docs · 3 files · ~3 KB · ~0.8k

**C18** - FromEnv without LINEAR_API_KEY or without MAQUINISTA_LINEAR_TEAM_ID yields Enabled=false (LB-05, AC 18)
Proof: `go test ./internal/pipeline/ -run TestBridgeConfig_Disabled -v`

**C19** - With both set: Enabled=true, PollInterval 60 s default, MAQUINISTA_LINEAR_POLL overrides (LB-05, AC 19)
Proof: `go test ./internal/pipeline/ -run TestBridgeConfig_Enabled -v`

**C20** - cmd_start starts claim + sync goroutines behind the gate and logs "pipeline: linear bridge started" (LB-05, AC 20)
Proof: `rg -n -e 'RunBridge' -e 'RunSync' -e 'pipeline: linear bridge started' cmd/maquinista/cmd_start.go`

**C21** - arch/pipeline.md documents the claim loop, the sync loop and the status mapping (LB-05, AC 21)
Proof: `rg -n -e 'ClaimIssue' -e 'linearSync' -e 'In Review' arch/pipeline.md`

**C22** - arch/README.md indexes the pipeline concern (LB-05, AC 22)
Proof: `rg -n 'pipeline' arch/README.md`

## Coverage

| Set (size) | Member -> proof | Unproven |
| --- | --- | --- |
| linear_issue_map DDL (1 table) | full column set of AC 1 via C1, table-driven over information_schema.columns | - |
| status→state mapping (7) | ready C13 · claimed C13 · review C13 · pending_approval C13 · failed C13 · done C13 · pending-unmapped C13+C17 | - |
| claim idempotency (2 calls) | first C9/C10 · second C11 | - |
| scheduler claim filters (2) | status ready C12 · no live agent C12 (project filter is the AtomicClaim path, not this query) | - |
| push outcomes (2) | success C14 · failure C15 | - |
| pending_state precedence (2) | unsynced explicit survives C16 · synced derived overwrites C16 | - |
| env gate (3 combos) | no key C18 · no team C18 · both set C19 | - |
| poll interval (2) | 60 s default C19 · MAQUINISTA_LINEAR_POLL override C19 | - |
| docs surfaces (2) | arch/pipeline.md C21 · arch/README.md C22 | - |

- The live end-to-end pilot (real MAQ board, real scheduler spawn) is plan open
  question 1 (EX-07 scope), not a coverage member - it needs the real Linear team
  and barceloneta env vars.
- No check claims more than the cases its proof exercises; C12 runs the scheduler
  SELECT verbatim rather than a reimplementation of it.

## Test policy

| Code | Required proofs | Coverage expectation |
| --- | --- | --- |
| Decides, reached across a boundary (GraphQL documents, state pushes) | one at its own layer over an httptest stub / fake client | one asserted case per decision-table row (fetch document 1, set-state 1, push 2) |
| Decides on stored data (claim tx, reconcile diff, backoff) | container-backed DB proof (dbtest.PgContainer) | one asserted case per row of each decision table (columns 11, statuses 7, precedence 2) |
| Entry point that decides nothing (cmd_start goroutine wiring) | one boundary proof (rg over the wiring lines) | wiring lines present; no behavioral enumeration |
| Docs | existence proofs (rg) | named sections present |

Evidence:

- `internal/relay/relay.go` + `internal/inboxecho/dispatch.go`: poll-loop components
  with `Run(ctx, pool, ...)` - already proven at unit layer in this repo without
  live Telegram. Same shape for both pipeline loops.
- `internal/db/migration_009_test.go`: migration-applies + information_schema
  asserts over PgContainer - the exact harness C1-C3 reuse.
- `internal/pipeline/linear.go`: net/http + GraphQL POST - decided fully by the
  stub server; no live Linear credential in tests.

Cost: 17 unit/container proofs across 4 new test files, 5 rg command proofs.
Without these rows the claim transaction and the sync precedence rule would be
proven only by a path that happens to traverse them.

## Swept

- validation: C5, C6 (malformed API answers never become silent successes), C9 (title/metadata shape pinned)
- failure modes: C15 (Linear outage degrades to backoff, not data loss), C17 (unmapped status skips instead of mis-pushing)
- idempotency: C11 (re-claim inserts nothing)
- authorization: n/a - the bridge acts as the integration credential; no user-facing surface (plan Surface: None)
- concurrency: C12 (SKIP LOCKED scheduler query verified against the inserted row); claim atomicity via PK/UNIQUE constraints (plan door 2)
- data lifecycle: C3 (cascade on task delete); no other retention rule - map rows live and die with their task
- dependency failure: C15 (client error path), C6 (HTTP-level errors)
- state transitions: C13-C16 (the mapping and precedence rules are the feature)
- observability: C20 (startup log line); per-tick errors log-and-continue following the orchestrator tick pattern

## Handoff

Arithmetic: new/modified bytes across S1-S5 ≈ 19 KB ≈ 5k tokens against the
150k budget - a single builder takes the whole feature, no split.

- **Boundary:** C1-C22 closed at `<sha>`
- **Settled mid-build:** none yet
- **Abandoned:** none yet
