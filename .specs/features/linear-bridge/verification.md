# Verification record — linear-bridge

Feature: linear-bridge (ADR-0005 EX-01 — Linear MAQ Todo issues become tasks; linearSync mirrors task state back)
Boundary commit: `8bddd9dc03cb3560c1184a5370bc81c0dd32616b` (branch `linear-bridge`; base `f03e05e47812b4fc93a76ae71e5ffc18424cc3fb` + 6 feature commits: 08a8d1a, ccf0e81, dfb6858, 356772e, 63059ce, 436a656, 8bddd9d)
Date: 2026-10-01
Verifier: independent (Hermes Verifier subagent; did not author the feature code)
Profile: light
Round: 1 (initial full verification, base..HEAD)

**Verdict:** PASS

## Method

All 22 proof commands were re-run verbatim from the worktree root with
`bash -c '<proof>; echo rc=$?'`, exit codes captured pipe-free. Docker
29.1.3 was available on the box, so all 12 container-backed proofs
(dbtest.PgContainer + full migration chain) executed for real — zero
container skips. Go 1.25.0. Supplementary signals: `go build ./...`
rc=0, `go vet ./internal/pipeline/ ./internal/db/` rc=0,
`go test ./internal/pipeline/ ./internal/db/` (whole packages) rc=0,
`TestBackoffDelay` rc=0. Beyond running the proofs, the implementation
was audited line-by-line against the AC wording (findings below), not
merely against what the tests assert.

## Per-check results

| C# | Claim re-derived | Result | Evidence |
| --- | --- | --- | --- |
| C1 | linear_issue_map full column set of AC 1 (11 cols, types/nullability/defaults, TEXT PK, UNIQUE(task_id) REFERENCES tasks ON DELETE CASCADE) | PASS | TestMigration034_LinearIssueMap rc=0; internal/db/migration_034_test.go:11; internal/db/migrations/034_linear_bridge.sql:5 |
| C2 | tasks.review_rounds INTEGER NOT NULL DEFAULT 0 | PASS | TestMigration034_ReviewRounds rc=0; internal/db/migrations/034_linear_bridge.sql:20 |
| C3 | task delete cascades to linear_issue_map; second map row for same task_id refused | PASS | TestMigration034_Cascade rc=0; internal/db/migration_034_test.go:119 |
| C4 | Authorization header = raw API key, no Bearer; Content-Type application/json | PASS | TestLinearClient_AuthHeaders rc=0; internal/pipeline/linear.go:68 |
| C5 | GraphQL errors array → error text contains first message | PASS | TestLinearClient_GQLErrors rc=0; internal/pipeline/linear.go:93 |
| C6 | non-200 HTTP → error contains the status code | PASS | TestLinearClient_HTTPStatus rc=0; internal/pipeline/linear.go:80 |
| C7 | fetch filters team id + workflow state "Todo" + label "pipeline", selects id/identifier/title/description/url | PASS | TestLinearClient_FetchTodoDocument rc=0; internal/pipeline/linear.go:112 |
| C8 | state update on 200 unwraps data envelope, returns resulting state name | PASS | TestLinearClient_SetIssueState rc=0; internal/pipeline/linear.go:150 |
| C9 | ClaimIssue same tx inserts tasks row status "ready", project_id from config, "[<identifier>] " title, metadata linear_issue_id+linear_url | PASS | TestClaim_InsertsTaskRow rc=0; internal/pipeline/bridge.go:65 |
| C10 | same tx inserts map row pending_state "In Progress", last_synced_state NULL, attempts 0 | PASS | TestClaim_InsertsMapRow rc=0; internal/pipeline/bridge.go:73 |
| C11 | re-claim returns created=false, exactly one tasks row, unchanged map row | PASS | TestClaim_Idempotent rc=0; internal/pipeline/bridge.go:82 |
| C12 | after commit the literal scheduler ready-claim SELECT matches the new task (role NULL → implementor fallback) | PASS | TestClaim_SchedulerQueryMatches rc=0; internal/pipeline/bridge_test.go:181 (query verified identical to internal/taskscheduler/taskscheduler.go:94) |
| C13 | DerivedState: ready+claimed→In Progress, review→In Review, pending_approval+failed→Needs Human, done→Done, other→empty | PASS | TestSync_DerivedState rc=0 (7 statuses); internal/pipeline/sync.go:24 |
| C14 | due row (pend≠last, next_attempt_at due) pushed; last_synced_state set, attempts 0, synced_at NOW | PASS | TestSync_PushesDueRow rc=0; internal/pipeline/sync.go:168 |
| C15 | failed push: attempts+1, next_attempt_at = NOW + backoff (15 s base doubling, 10 m cap), last_synced_state unchanged | PASS | TestSync_BackoffOnFailure rc=0; internal/pipeline/sync.go:41 (table re-checked by TestBackoffDelay rc=0) |
| C16 | derived overwrite only while previous pending_state already synced; unsynced explicit pending survives | PASS | TestSync_DerivedOverwriteRule rc=0 (both branches); internal/pipeline/sync.go:110 |
| C17 | mapped task with unmapped status skipped, row unpushed, map row untouched | PASS | TestSync_SkipsUnmappedStatus rc=0; internal/pipeline/sync.go:113 |
| C18 | FromEnv without LINEAR_API_KEY or without MAQUINISTA_LINEAR_TEAM_ID → Enabled=false (3 combos) | PASS | TestBridgeConfig_Disabled rc=0; internal/pipeline/bridge.go:29 |
| C19 | both set → Enabled=true, 60 s default, MAQUINISTA_LINEAR_POLL overrides (invalid override falls back to 60 s) | PASS | TestBridgeConfig_Enabled rc=0; internal/pipeline/bridge.go:44 |
| C20 | cmd_start starts claim + sync goroutines behind the gate, logs "pipeline: linear bridge started" once | PASS | rg rc=0 — cmd/maquinista/cmd_start.go:466, cmd/maquinista/cmd_start.go:471, cmd/maquinista/cmd_start.go:475 |
| C21 | arch/pipeline.md documents claim loop, sync loop, status mapping table | PASS | rg rc=0 — arch/pipeline.md:16, arch/pipeline.md:27, arch/pipeline.md:39 |
| C22 | arch/README.md lists the pipeline concern pointing at arch/pipeline.md | PASS | rg rc=0 — arch/README.md:20 |

Tally: 22 PASS, 0 other verdicts. No proof skipped.

## Coverage recompute

Recomputed from the authority sets in checks.md, not copied:

| Set (size) | Evidence | Unproven |
| --- | --- | --- |
| linear_issue_map DDL (11 columns, PK, UNIQUE) | C1 (information_schema + constraint counts) | - |
| status→state mapping (7 rows) | C13 (6 mapped) + C17 (unmapped skip) | - |
| claim idempotency (first / second call) | C9, C10, C11 | - |
| scheduler claim filters (status ready, no live agent) | C12 (literal query verbatim) | - |
| push outcomes (success, failure) | C14, C15 | - |
| pending_state precedence (explicit unsynced wins; derived fills when pend empty or pend==last) | C16 (both branches) | - |
| env gate (key absent, team absent, both present) | C18, C19 | - |
| poll interval (60 s default, override, invalid override) | C19 | - |
| docs surfaces (arch/pipeline.md, arch/README.md) | C21, C22 | - |

Plan open question 1 (live end-to-end pilot on the real MAQ board) is
EX-07 scope and not a coverage member, per checks.md.

## Independent audit

Adversarial findings from reading the implementation against the AC
wording (none blocking; nothing changes the verdict):

- LOW — internal/pipeline/sync.go:110 — a row holding an explicit
  unsynced pending_state is pushed even when its task status is
  unmapped. Read hyper-literally, AC 17 ("IF a mapped task carries a
  status with no mapping THEN the syncer SHALL skip the row leaving it
  unpushed") could forbid this; AC 16 and the plan's design ("sync
  accepts explicit states but adds none") make the explicit pending
  authoritative, and the C17 proof seeds pend empty so the check is
  unambiguous. Design-consistent; wording ambiguity only.
- LOW — internal/pipeline/bridge.go:76 — ON CONFLICT targets only the
  PK (linear_issue_id); a UNIQUE(task_id) violation would surface as an
  error rather than created=false. Unreachable under current call
  sites because task_id is a fresh UUIDv4 per claim (bridge.go:64).
- INFO — internal/pipeline/sync.go:130 — derived pending_state UPDATEs
  land before the WorkflowStates fetch (sync.go:141); if that fetch
  errors, the writes persist and the transition re-pushes next tick
  (pend≠last still holds) — diff-based design keeps no transition
  lost. Consistent with AC 15's outage posture.
- INFO — internal/pipeline/sync.go:120 — due-ness compares the
  DB-written next_attempt_at against the app's time.Now while the DB's
  clock writes it (sync.go:163); cross-host skew shifts a retry by the
  skew amount only. Harmless.
- INFO — internal/pipeline/sync.go:159 — the backoff and success
  UPDATEs do not bump updated_at (only the derived write does,
  sync.go:132). Bookkeeping inconsistency; no AC references updated_at.
- INFO — internal/pipeline/linear.go:150 — the push method is named
  UpdateIssueState while AC 8/AC 14 and check C8 say "SetIssueState";
  behavior is identical (unwraps envelope, returns resulting state
  name). Naming-only divergence from the spec prose.
- INFO — internal/pipeline/bridge.go:66 — with neither
  MAQUINISTA_LINEAR_PROJECT nor MAQUINISTA_PROJECT set, claimed tasks
  get project_id '' (the Go empty string binds as '', not NULL;
  tasks.project_id is nullable TEXT per 001_initial.sql:15). AC 9 says
  "project_id from config", and the scheduler ready-claim query does
  not filter on project, so behavior is unaffected.

Audit-positive confirmations (claims re-derived from code, not just
tests): claim rollback correctness — the created=false path returns
before Commit and the deferred Rollback (bridge.go:62) discards the
task INSERT, which TestClaim_Idempotent proves by asserting exactly one
tasks row; backoff arithmetic is overflow-safe (shift clamped at 6,
sync.go:46, so 15 s × 2⁶ = 16 min caps to 10 min; first failure = 15 s,
doubling per stored attempts); the precedence rule at sync.go:111 is
exactly "derived fills only when pend=='' or pend==last"; the env gate
(bridge.go:29) requires BOTH variables and cmd_start.go:463 gates on
Enabled() with pool non-nil, logging the startup line exactly once;
Authorization carries the raw key (no Bearer prefix) at linear.go:68;
migration 034 is additive and is the latest in the chain (033 → 034).

## Pre-existing failures

Out of scope for this feature (excluded from every C1-C22 verdict):

- TestOutboxSink_WritesAssistantText — internal/monitor
- TestOutboxSink_WritesThinking — internal/monitor
- TestToolEventSink_PairedEmitsBoth — internal/monitor
- TestPiSource_DiscoverBackfill — internal/monitor

Status on the feature tree: package ./internal/monitor with those four
selected tests exits rc=1 (reproduced 2026-10-01). They were reported
as pre-existing and reproduce at clean base, which this verifier
confirmed directly: the working tree was clean with the feature fully
committed, so the suggested stash spot-check was a no-op by
construction; instead a throwaway git worktree was created at base
f03e05e (which predates all six feature commits) and the four tests
failed there identically, rc=1. The monitor package references neither
internal/pipeline nor the cmd_start wiring (rg over internal/monitor:
zero hits), so no plausible interaction exists. The temp worktree was
removed and pruned; the feature worktree and its HEAD are untouched.
