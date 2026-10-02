# Verification — role-souls

Independent verifier run, 02/10/2026. Worktree `maquinista.role-souls`, branch `role-souls`
at 3b840c0 (base 5b66aee, +3 commits). Every proof below was executed from the worktree
root by the verifier; rc captured `; echo rc=$?`. Rendered-contract claims (C4–C12) were
additionally cross-checked against the migration SQL text, not just test names.

## Verification

| C# | Proof (short) | Result | Evidence |
| --- | --- | --- | --- |
| C1 | `go test ./internal/soul/ -run TestMigration035_Seed -v` | PASS | `--- PASS: TestMigration035_Seed`, rc=0; 5 pipeline templates load, is_default false, count `LIKE 'pipeline-%'` = 5 |
| C2 | `go test ./internal/soul/ -run TestMigration035_Idempotent -v` | PASS | `--- PASS: TestMigration035_Idempotent`, rc=0; re-applied 035 by hand — row count and legacy snapshots (default/coordinator/planner/coder) identical |
| C3 | `go test ./internal/soul/ -run TestPipelineTemplateClone -v` | PASS | `--- PASS: TestPipelineTemplateClone`, rc=0; clone carries role/goal/core_truths/boundaries/extras verbatim, template_id=pipeline-worker |
| C4 | `go test ./internal/soul/ -run TestPipelineWorkerContract -v` | PASS | `--- PASS: TestPipelineWorkerContract`, rc=0; rendered prompt contains validate_plan.py, validate_checks.py, .specs/features/, "before any implementation code" — SQL 035:32-33 matches |
| C5 | `go test ./internal/soul/ -run TestPipelineWorkerContract -v` | PASS | same run as C4; maquinista-done/-observe/-handoff, check_commit.py, "runs green at HEAD" all in rendered prompt — SQL 035:34,36 matches |
| C6 | `go test ./internal/soul/ -run TestPipelineWorkerContract -v` | PASS | same run; boundaries forbid out-of-task scope and checks.md gaming — SQL 035:37-38 matches |
| C7 | `go test ./internal/soul/ -run TestPipelineReviewerContract -v` | PASS | `--- PASS: TestPipelineReviewerContract`, rc=0; task+plan.md+checks.md+verification.md+full diff required, builder-summary prohibition — SQL 035:49,54 matches |
| C8 | `go test ./internal/soul/ -run 'TestPipelineReviewerContract\|TestPipelineArbiterContract' -v` | PASS | both `--- PASS` lines present in one run, rc=0; all three literals `VERDICT: approve` / `VERDICT: request_changes` / `VERDICT: needs_human` asserted on rendered output — SQL 035:52,67 "nothing after it" |
| C9 | `go test ./internal/soul/ -run TestPipelineArbiterContract -v` | PASS | `--- PASS: TestPipelineArbiterContract`, rc=0; contested/repeated request_changes adjudication, file:line evidence, same verdict vocabulary — SQL 035:64,67 matches |
| C10 | `go test ./internal/soul/ -run TestPipelineFixerContract -v` | PASS | `--- PASS: TestPipelineFixerContract`, rc=0; same worktree/PR, findings-list discipline, no scope expansion, no checks.md/spec-obligation edits — SQL 035:79,83-84 matches |
| C11 | `go test ./internal/soul/ -run TestPipelineMergerContract -v` | PASS | `--- PASS: TestPipelineMergerContract`, rc=0; origin/main rebase, gh pr checks gate, "conflict file list" parking — SQL 035:94-98 matches |
| C12 | `go test ./internal/soul/ -run TestPipelineMergerContract -v` | PASS | same run as C11; "merge proposal" + maquinista approve wait stated as default (PIPELINE_AUTO_MERGE=0) — SQL 035:94,97 matches |
| C13 | `go test ./internal/soul/ -run TestPipelineDispatchExtras -v` | PASS | `--- PASS: TestPipelineDispatchExtras`, rc=0; default_runner=pi on all five; reasoning_class standard on worker/fixer/merger, high on reviewer/arbiter — SQL 035:42,57,72,87,102 matches |
| C14 | `rg -c '## Role souls' arch/pipeline.md` + `'VERDICT: '` + `'default_runner'` | PASS | counts 1, 2, 1 with rc=0; arch/pipeline.md:78-103 lists five templates, verdict contract, extras keys |
| C15 | build+vet, gofmt, validators | PASS | `go build ./... && go vet ./...` rc=0; `gofmt -l` on pipeline_souls_test.go + migrations/ lists nothing rc=0; validate_plan rc=0 (0 err/0 warn); validate_checks rc=0 (0 err, 1 warning — see Notes) |
| C16 | `go test ./internal/soul/ -run TestPipelineTemplateClone -v` | PASS | same proof as C3, run once, cited for both; clone read-back via Load exposes decoded extras — the AC 15 regression (jsonb scan) is covered |
| Gates | build / vet / gofmt / soul suite / validators | PASS | all rc=0 as under C15; soul suite green across 9 selector runs (testcontainers postgres:16-alpine each) |

## Notes

- **Test-assertion weakness (minor, C10):** the fixer test asserts generic `"findings"`
  (pipeline_souls_test.go:292) rather than the AC's "numbered findings". The SQL text does
  say "reviewer numbered findings" (035_seed_pipeline_souls.sql:79), so the material claim
  holds — verified by reading the SQL — but the automated selector alone would not catch a
  regression that drops "numbered".
- **Template-field vs rendered assertions (minor, C6/C7/C9/C10/C12):** several boundary/goal
  checks assert on `tpl.Boundaries` / `tpl.Goal` (e.g. pipeline_souls_test.go:207-212, 234,
  257-262, 289, 295-299, 324) instead of the rendered prompt. Materially equivalent because
  Render always emits Goal and Boundaries sections (soul.go:292, 297-300) and these tests
  also render the clone uncapped (`Render(*s, 0)`) for their remaining assertions.
- **validate_checks warning on C15:** `proof names no test selector` — inherent to
  gate-style proofs (build/vet/gofmt); validator still rc=0 with 0 errors.
- **C3/C16 share one proof command** per checks.md; executed once, cited for both rows.
- **Load fix confirmed correct** (soul.go:234, 255): extras now scanned as `[]byte` and
  unconditionally passed to `decodeExtras`; previously an `any` + `.([]byte)` assertion
  silently emptied extras since pgx v5 returns `map[string]any` for jsonb-into-`any`.
- Pre-existing failures (internal/monitor tests, gofmt on compose.go/compose_test.go) were
  not re-run/re-checked — C15 scopes gofmt to touched files and no proof runs those
  packages, per checks.md.

## Verdict

All 16 checks pass with their named tests as `--- PASS` and rc=0, and the rendered
contract texts in `035_seed_pipeline_souls.sql` materially match every claim in C4–C13:
the three verdict-line literals are exact on reviewer and arbiter, the spec-first rule
names both validators and the .specs path, the worker/fixer/merger/reviewer boundaries
carry the promised prohibitions, and the extras dispatch keys pair pi+standard /
pi+high exactly as specified. The docs section and the Load read-back fix are in place.
The only findings are two minor test-assertion weaknesses that do not change any outcome.

VERDICT: approve
