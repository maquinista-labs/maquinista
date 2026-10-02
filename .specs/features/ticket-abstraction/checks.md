# Checks — ticket-abstraction (light)

Profile: light

Boundary: PR #1 branch `linear-bridge`, head at the S3 docs boundary commit; verification
lands after. All proofs run from the worktree root; rc captured pipe-free (`; echo rc=$?`).

## Checks

**C1** - `034_ticket_bridge.sql` exists, `034_linear_bridge.sql` does not, and
`ticket_issue_map` carries the full column set of AC 1 (issue_id TEXT PK, issue_key NOT
NULL, team_id NOT NULL, task_id NOT NULL UNIQUE REFERENCES tasks ON DELETE CASCADE,
bookkeeping columns + defaults) (TA-01, AC 1)

Proof: `test -f internal/db/migrations/034_ticket_bridge.sql && ! test -f internal/db/migrations/034_linear_bridge.sql; echo rc=$?` and `go test ./internal/db/ -run TestMigration034_TicketIssueMap -v`

**C2** - `tasks.review_rounds` INTEGER NOT NULL DEFAULT 0, still proven (TA-01, AC 2)

Proof: `go test ./internal/db/ -run TestMigration034_ReviewRounds -v`

**C3** - Deleting a tasks row removes its ticket_issue_map row (cascade) (TA-01, AC 3)

Proof: `go test ./internal/db/ -run TestMigration034_Cascade -v`

**C4** - No 'linear' occurrence anywhere under internal/db/ (TA-01, AC 4)

Proof: `rg -i linear internal/db/; echo rc=$?`

**C5** - provider.go defines TicketProvider (IntakeIssues/Columns/SetIssueColumn), Issue and
the Column enum whose String() returns the five canonical names (TA-02, AC 5)

Proof: `go test ./internal/pipeline/ -run TestColumn -v`

**C6** - Under internal/pipeline/ exactly linear.go and linear_test.go match 'linear'
(TA-02, AC 6)

Proof: `rg -il linear internal/pipeline/ | sort; echo rc=$?`

**C7** - Linear provider keeps transport semantics (raw Authorization, envelope unwrap,
GQL/HTTP errors, Todo+pipeline intake filter) and resolves MAQUINISTA_TICKETS_API_KEY with
LINEAR_API_KEY fallback (TA-02, AC 7)

Proof: `go test ./internal/pipeline/ -run TestLinearProvider -v`

**C8** - NewProvider("linear") returns a provider; unknown names error listing supported
providers (TA-02, AC 8)

Proof: `go test ./internal/pipeline/ -run TestNewProvider -v`

**C9** - ClaimIssue stamps metadata ticket_issue_id + ticket_url, "[<Key>] " title prefix,
ticket_issue_map row pending "In Progress"; re-claim no-op; scheduler query matches (TA-02,
AC 9)

Proof: `go test ./internal/pipeline/ -run TestClaim -v`

**C10** - Sync stores canonical names only; explicit-pending precedence, diff due-ness,
backoff 15 s doubling / 10 min cap, unmapped skip all preserved (TA-02, AC 10)

Proof: `go test ./internal/pipeline/ -run TestSync -v`

**C11** - Config is neutral: MAQUINISTA_TICKETS_* contract, gate requires key + team,
60 s default with MAQUINISTA_TICKETS_POLL override, project fallback chain; no
MAQUINISTA_LINEAR_* under internal/ or cmd/ (TA-03, AC 11)

Proof: `go test ./internal/pipeline/ -run TestTicketsConfig -v` and `rg -i 'MAQUINISTA_LINEAR_' internal/ cmd/; echo rc=$?`

**C12** - cmd_start builds the provider via NewProvider and contains no 'linear' (TA-03,
AC 12)

Proof: `rg -n 'NewProvider' cmd/maquinista/cmd_start.go` and `rg -i linear cmd/; echo rc=$?`

**C13** - arch/pipeline.md is provider-neutral with the MAQUINISTA_TICKETS_* env table,
ADRs/0006-ticket-provider-abstraction.md exists, ADRs/README.md indexes it (TA-03, AC 13)

Proof: `test -f ADRs/0006-ticket-provider-abstraction.md && rg -c 'MAQUINISTA_TICKETS_' arch/pipeline.md; echo rc=$?` and `rg -n '0006' ADRs/README.md`

**C14** - Full gates green: build, vet, gofmt on touched files, pipeline+db suites; spec
validators rc=0 (S4)

Proof: `go build ./... && go vet ./...; echo rc=$?` and `python3 ~/.hermes/skills/tlc-spec-lean/scripts/validate_plan.py ticket-abstraction; echo rc=$?` and `python3 ~/.hermes/skills/tlc-spec-lean/scripts/validate_checks.py ticket-abstraction; echo rc=$?`

## Known pre-existing failures (out of scope)

- `internal/monitor`: TestOutboxSink_WritesAssistantText, TestOutboxSink_WritesThinking,
  TestToolEventSink_PairedEmitsBoth, TestPiSource_DiscoverBackfill — reproduce at base
  `f03e05e`; documented in linear-bridge verification.md. Unrelated package.

## Swept

- **validation**: NewProvider rejects unknown names with the supported list (C8); config
  parsing keeps duration/gate guards (C11)
- **failure modes**: provider GQL/HTTP failures surface as errors, never panics (C7);
  unmapped column names degrade to backoff, not hot-loop (C10)
- **idempotency**: claim remains INSERT-first ON CONFLICT no-op (C9); reconcile is
  diff-based so repeated ticks are no-ops when synced (C10)
- **authorization**: raw Authorization header, no Bearer, unchanged through the provider
  seam (C7); key resolution MAQUINISTA_TICKETS_API_KEY → LINEAR_API_KEY (C7)
- **concurrency**: PK-INSERT single-owner claim unchanged (C9); one sync goroutine per
  process, same as linear-bridge revision
- **data lifecycle**: ticket_issue_map rows cascade with tasks (C3); no new retention
- **dependency failure**: Linear outage → exponential backoff 15 s doubling, 10 min cap,
  state survives restarts in the map row (C10)
- **state transitions**: only canonical column names stored; explicit-pending precedence
  and derived-fill-only-when-synced preserved (C10)
- **observability**: startup log line per bridge enable (C12); push failures logged with
  issue + state, same shape as before (existing guard: `pipeline:` log prefix)

## Coverage

| Set (size) | Member -> proof | Unproven |
| --- | --- | --- |
| ticket_issue_map DDL (1 table) | full column set via C1, table-driven over information_schema.columns | - |
| review_rounds + cascade (2) | review_rounds C2 · cascade C3 | - |
| neutrality negatives (3 zones) | internal/db C4 · internal/pipeline C6 · cmd/ C12 | - |
| canonical columns (5) | InProgress C5 · InReview C5 · ChangesRequested C5 · NeedsHuman C5 · Done C5 | - |
| provider transport (4 behaviors) | auth C7 · envelope C7 · errors C7 · intake filter C7 | - |
| factory (2 paths) | linear C8 · unknown C8 | - |
| claim semantics (3) | insert+metadata C9 · idempotent C9 · scheduler match C9 | - |
| sync semantics (4) | canonical push C10 · precedence C10 · backoff C10 · unmapped skip C10 | - |
| env gate (3 combos) | no key C11 · no team C11 · both set C11 | - |
| docs surfaces (3) | arch/pipeline.md C13 · ADR-0006 C13 · ADRs/README C13 | - |
| gates (4) | build+vet C14 · gofmt C14 · suites C14 · validators C14 | - |
