# Verification record — ticket-abstraction

Feature: ticket-abstraction (ADR-0006 — PR #1 pre-merge revision: pipeline core
neutralized behind a TicketProvider seam; Linear demoted to one provider)
Boundary commit: `98cbbfdd660660b87abf3896457c66bd2c365b3c` (branch `linear-bridge`;
base `b15954d` + 6 feature commits: 0835ae2, 08210e4, 4a78db6, d5251f0 (round-1
boundary), 2d536b4, 98cbbfd)
Date: 2026-10-02
Verifier: independent (Hermes Verifier subagent; did not author the feature code)
Profile: light
Round: 2 (re-verification after round-1 FAIL 12/14 — C6, C7; fixes verified at 98cbbfd)

**Verdict:** PASS (14/14)

## Method

All 20 proof commands from checks.md (C1–C14, counting each command in
multi-command proofs separately) were re-run verbatim from the worktree root
with `; echo rc=$?`, exit codes captured pipe-free (command output redirected
to a file, then inspected — never through a pipe). Docker 29.1.3 was
available on the box, so the 12 container-backed proof invocations executed
for real — 12 postgres:16-alpine containers spun up (one per DB-hitting test:
3 in the db proofs, 4 in TestClaim, 5 in TestSync), ryuk reaper included,
zero skips. Go 1.25.0. Supplementary signals: `gofmt -l` over the touched
packages, `go test -count=1` for both suites (db uncached 42.6 s, pipeline
uncached 18.9 s), the monitor pre-existing-failure reproduction, and a
base-revision reproduction of every excluded failure (throwaway worktree at
base `b15954d`, removed after — see Pre-existing failures). Beyond running
the proofs, the round-1 fixes were audited in the diff (2d536b4, 98cbbfd)
and against the AC wording; no code or spec file was modified by this
verification.

## Round-1 failures and fixes (context for this round)

Round 1 (at head d5251f0) failed 12/14:

- **C7** — the proof regex `TestLinearProvider` selected nothing
  ("no tests to run"); transport tests still carried their old
  `TestLinearClient_*` names, and the MAQUINISTA_TICKETS_API_KEY →
  LINEAR_API_KEY fallback was implemented but untested anywhere.
  **Fix (2d536b4):** the five transport tests renamed
  TestLinearClient_* → TestLinearProvider_* and a new
  TestLinearProvider_KeyFallback added.
- **C6** — the neutrality negative `rg -il linear internal/pipeline/`
  listed 7 files; AC 6 as written was jointly unsatisfiable with AC 8/AC 11
  because the factory key and config default `"linear"` are core config
  data. **Fix (98cbbfd):** spec re-scoped — plan.md AC 6 and checks.md C6 now
  target vendor-transport symbols (`LINEAR_API_KEY`, api.linear,
  LinearClient, GraphQL), matching exactly linear.go + linear_test.go; the
  provider name `"linear"` as factory key/config default is explicitly
  allowed in core; ADR-0006's negative-check bullet updated to match.

Both fixes verified directly this round (results below).

## Per-check results

| C# | Claim re-derived | Result | Evidence |
| --- | --- | --- | --- |
| C1 | 034_ticket_bridge.sql exists, 034_linear_bridge.sql gone; ticket_issue_map full AC 1 column set (11 cols, types/nullability/defaults, TEXT PK, UNIQUE(task_id) REFERENCES tasks ON DELETE CASCADE) | PASS | file-proof rc=0; TestMigration034_TicketIssueMap rc=0 (table-driven over information_schema, container real); internal/db/migrations/034_ticket_bridge.sql |
| C2 | tasks.review_rounds INTEGER NOT NULL DEFAULT 0, still proven | PASS | TestMigration034_ReviewRounds rc=0, container real; internal/db/migrations/034_ticket_bridge.sql:24 |
| C3 | task delete cascades to ticket_issue_map; second map row for same task_id refused | PASS | TestMigration034_Cascade rc=0, container real; internal/db/migration_034_test.go:119 |
| C4 | zero 'linear' occurrences under internal/db/ | PASS | rg rc=1 with empty output (rg's no-match exit IS the pass signal here) |
| C5 | TicketProvider (IntakeIssues/Columns/SetIssueColumn), Issue{ID,Key,Title,Description,URL}, Column enum with 5 canonical String() names | PASS | TestColumn_String rc=0 (5 names + columnFromName roundtrip + unknown→""); internal/pipeline/provider.go |
| C6 | under internal/pipeline/ exactly linear.go and linear_test.go match vendor-transport symbols (LINEAR_API_KEY, api.linear, LinearClient, GraphQL); "linear" as factory key/config default allowed in core | PASS | proof lists **exactly** internal/pipeline/linear.go + internal/pipeline/linear_test.go, rc=0 (round 1 listed 7 files; re-scoped proof confirms) |
| C7 | Linear provider transport semantics kept (raw Authorization, envelope unwrap, GQL/HTTP errors, Todo+pipeline intake filter) + MAQUINISTA_TICKETS_API_KEY with LINEAR_API_KEY fallback | PASS | TestLinearProvider -v runs **6/6 PASS** rc=0 — AuthHeaders, GQLErrors, HTTPStatus, FetchTodoDocument, SetIssueState, **KeyFallback**; zero "no tests to run" warnings (round 1: vacuous, 0 tests) |
| C8 | NewProvider("linear") returns a provider; unknown names error listing supported | PASS | TestNewProvider rc=0; internal/pipeline/provider.go:86, :94 ("supported: linear") |
| C9 | claim stamps ticket_issue_id/ticket_url metadata, "[<Key>] " title, map row pending "In Progress"; re-claim no-op; scheduler query matches | PASS | TestClaim 4/4 rc=0, containers real (4 test containers); internal/pipeline/bridge.go |
| C10 | sync stores canonical names only; explicit-pending precedence, diff due-ness, 15 s doubling/10 min cap backoff, unmapped skip preserved | PASS | TestSync 6/6 rc=0, containers real (5 test containers); internal/pipeline/sync.go |
| C11 | MAQUINISTA_TICKETS_* contract, gate key+team, 60 s default + override, project fallback chain; no MAQUINISTA_LINEAR_* under internal/ or cmd/ | PASS | TestTicketsConfig_Disabled/Enabled rc=0 (gate combos + fallback directions); rg 'MAQUINISTA_LINEAR_' internal/ cmd/ rc=1 (no matches) |
| C12 | cmd_start builds provider via NewProvider, contains no 'linear' | PASS | rg NewProvider rc=0 (cmd/maquinista/cmd_start.go:465); rg -i linear cmd/ rc=1 (no matches) |
| C13 | arch/pipeline.md provider-neutral with MAQUINISTA_TICKETS_* table, ADR-0006 exists, ADRs/README.md indexes it | PASS | proof1 rc=0 (rg -c count 8); proof2 rc=0 (ADRs/README.md:16); arch/README.md:20 additionally lists pipeline.md "(provider-neutral, ADR-0006)" |
| C14 | gates green: build, vet, gofmt on touched files, pipeline+db suites, spec validators rc=0 | PASS | go build ./... && go vet ./... rc=0; validate_plan rc=0 (0 errors, 0 warnings); validate_checks rc=0 (0 errors, 6 pre-existing warnings); both suites rc=0 uncached (pipeline 18.9 s, db 42.6 s); gofmt clean on all touched internal/pipeline/*.go + internal/db/migration_034_test.go (cmd/ + internal/db/queries drift is pre-existing — see below) |

Tally: 14 PASS, 0 FAIL. No proof skipped, no container skipped.

## Coverage recompute

Recomputed from the authority sets in checks.md, not copied:

| Set (size) | Evidence | Unproven |
| --- | --- | --- |
| ticket_issue_map DDL (1 table) | C1 (information_schema, table-driven + constraint counts) | - |
| review_rounds + cascade (2) | C2 · C3 | - |
| neutrality negatives (3 zones) | internal/db C4 · internal/pipeline C6 (exactly linear.go + linear_test.go) · cmd/ C12 | - |
| canonical columns (5) | C5 (names + roundtrip + unknown) | - |
| provider transport (4 behaviors) | C7 (auth/envelope/errors/intake under TestLinearProvider_*) | - |
| key fallback (2 directions) | C7 TestLinearProvider_KeyFallback (empty key → LINEAR_API_KEY; explicit MAQUINISTA_TICKETS_API_KEY wins) | - |
| factory (2 paths) | C8 · C8 | - |
| claim semantics (3) | C9 (insert+metadata · idempotent · scheduler match) | - |
| sync semantics (4) | C10 (canonical push · precedence · backoff · unmapped skip; non-canonical skip additionally proven) | - |
| env gate (3 combos) | C11 | - |
| docs surfaces (3) | C13 (plus arch/README.md:20 verified manually, beyond the proof) | - |
| gates (4) | build+vet C14 · gofmt C14 · suites C14 (uncached) · validators C14 | - |

Round 1's unproven cells (internal/pipeline neutrality zone; key-fallback
resolution) are both closed this round.

## Independent audit

Round-2-specific findings from auditing the fix commits and the re-scoped
spec (none blocking):

- CONFIRMED — TestLinearProvider_KeyFallback (linear_test.go:148) pins both
  directions: NewProvider("linear", "") with LINEAR_API_KEY=legacy-key yields
  client.APIKey == "legacy-key", and an explicit key wins over the env
  fallback. The implementation reads MAQUINISTA_TICKETS_API_KEY first and
  falls back to os.Getenv("LINEAR_API_KEY") only when the passed key is empty
  (linear.go:185-190).
- CONFIRMED — ADR-0006's negative-check bullet (ADRs/0006-ticket-provider-
  abstraction.md:80-81) now requires the vendor-transport symbols
  (LINEAR_API_KEY, the Linear API host, LinearClient, GraphQL) to match
  exactly linear.go + linear_test.go — identical wording to re-scoped AC 6
  and the C6 proof. Spec and ADR are consistent.
- CONFIRMED — commit 2d536b4 also reworded bridge.go's FromEnv comment to
  "the legacy Linear env key", removing the LINEAR_API_KEY literal from the
  comment so the C6 symbol list stays exact; behavior untouched.
- CONFIRMED — the provider name "linear" still appears in core files
  (bridge.go default fill, provider.go factory arm + error text,
  config_test.go/provider_test.go/bridge_test.go assertions), which re-scoped
  AC 6 explicitly permits as configuration data; the vendor-transport proof
  matches exactly the two provider files.
- INFO — carried over from round 1, re-checked at 98cbbfd: TestNewProvider
  asserts an unknown name errors but not that the message names the supported
  providers (satisfied in code, provider.go:94, not test-asserted);
  bridge.go:74 still builds task metadata with fmt.Sprintf + %q (JSON-valid
  for Linear-shaped values, not a general JSON encoder — pre-revision shape);
  due-ness still compares DB-written next_attempt_at against the app clock;
  the push method is still named UpdateIssueState behind SetIssueColumn
  (linear.go:157/:221, behavior unchanged).

Audit-positive confirmations (claims re-derived from code, not just tests):
the C7 transport behaviors assert the same things the round-1
TestLinearClient_* tests did (only names changed — verified in the 2d536b4
diff, bodies unchanged); the factory rejects unknown names with the
supported list; the env gate requires BOTH key and team; cmd_start gates on
Enabled() && pool != nil with the startup line logged exactly once
(cmd_start.go:464-479); the touched internal/pipeline/*.go (9 files) and
internal/db/migration_034_test.go are gofmt-clean; both suites pass uncached
(-count=1), so no cached-result masking.

## Pre-existing failures

Out of scope for this feature (excluded from every C1–C14 verdict):

- TestOutboxSink_WritesAssistantText — internal/monitor
- TestOutboxSink_WritesThinking — internal/monitor
- TestToolEventSink_PairedEmitsBoth — internal/monitor
- TestPiSource_DiscoverBackfill — internal/monitor

Status on the feature tree: the four selected tests exit rc=1 (reproduced
2026-10-02 at 98cbbfd, all four --- FAIL). Reproduced independently at the
feature base `b15954d` in a throwaway git worktree: identical 4 × --- FAIL,
rc=1. Consistent with round 1 (d5251f0) and the linear-bridge record (base
f03e05e). The monitor package references neither the pipeline seam nor the
cmd wiring. The throwaway worktree was removed and pruned; the feature
worktree and its HEAD are untouched.

gofmt drift likewise pre-existing and excluded: `gofmt -l cmd/` lists 12
files (including cmd/maquinista/cmd_start.go) plus internal/db/queries.go
and internal/db/queries_test.go. The identical 14-file list was verified at
base b15954d in the same throwaway worktree, so none of it originates in
this feature — even though commit 08210e4 touched cmd_start.go, it was
already unformatted there.

## Verdict

**PASS 14/14.** Round-1 failures C6 and C7 are fixed and verified: the
neutrality proof now matches exactly linear.go + linear_test.go, and the
TestLinearProvider_* suite (6/6, including the new KeyFallback pin) proves
the transport semantics and the legacy key fallback. All 20 proofs rc
green as specified; the only non-green signals are the documented
pre-existing monitor failures and gofmt drift, both reproduced at base
b15954d and out of scope. No code or spec file was modified by this
verification; this record is untracked for the parent session to commit.
