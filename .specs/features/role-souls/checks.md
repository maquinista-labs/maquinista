# Checks — role-souls (light)

Profile: light

Boundary: branch `role-souls`, base `5b66aee` (EX-01 squash merge). All proofs run from
the worktree root; rc captured pipe-free (`; echo rc=$?`). Go proofs: every named test
must appear as `--- PASS: <Name>` in the cited run — a green exit with no RUN lines is a
broken selector, not a pass.

## Checks

**C1** - Migration 035 seeds exactly the five pipeline templates (pipeline-worker,
pipeline-reviewer, pipeline-arbiter, pipeline-fixer, pipeline-merger), every one
is_default FALSE (RS-01, AC 1)

Proof: `go test ./internal/soul/ -run TestMigration035_Seed -v`

**C2** - Re-applying 035 is a no-op: row count unchanged and the pre-existing templates
(default, coordinator, planner, coder) identical before and after (RS-01, AC 2)

Proof: `go test ./internal/soul/ -run TestMigration035_Idempotent -v`

**C3** - CreateFromTemplate on pipeline-worker clones role, goal, core_truths, boundaries
and extras verbatim into a new agent_souls row (RS-01, AC 3)

Proof: `go test ./internal/soul/ -run TestPipelineTemplateClone -v`

**C4** - Rendered pipeline-worker prompt contains the spec-first rule: plan.md +
checks.md under .specs/features/<slug>/ passing validate_plan.py and validate_checks.py
before any implementation code (RS-02, AC 4)

Proof: `go test ./internal/soul/ -run TestPipelineWorkerContract -v`

**C5** - Rendered pipeline-worker prompt names maquinista-done, maquinista-observe,
maquinista-handoff and binds completion to all checks.md proofs green at HEAD +
check_commit.py per commit (RS-02, AC 5)

Proof: `go test ./internal/soul/ -run TestPipelineWorkerContract -v`

**C6** - Rendered pipeline-worker prompt forbids out-of-task scope and editing checks.md
claims to turn a red proof green (RS-02, AC 6)

Proof: `go test ./internal/soul/ -run TestPipelineWorkerContract -v`

**C7** - Rendered pipeline-reviewer prompt requires task + plan.md + checks.md +
verification.md + full diff and forbids verdict from the builder's summary alone (RS-03,
AC 7)

Proof: `go test ./internal/soul/ -run TestPipelineReviewerContract -v`

**C8** - Rendered reviewer and arbiter prompts require the final output line
`VERDICT: ` + exactly one of approve, request_changes, needs_human (RS-03, AC 8)

Proof: `go test ./internal/soul/ -run 'TestPipelineReviewerContract|TestPipelineArbiterContract' -v`

**C9** - Rendered pipeline-arbiter prompt defines high-reasoning adjudication of a
contested or repeated request_changes with file:line evidence and the same verdict
vocabulary (RS-03, AC 9)

Proof: `go test ./internal/soul/ -run TestPipelineArbiterContract -v`

**C10** - Rendered pipeline-fixer prompt scopes work to the reviewer's numbered findings
in the same worktree/PR and forbids spec-obligation edits and scope-expanding refactors
(RS-04, AC 10)

Proof: `go test ./internal/soul/ -run TestPipelineFixerContract -v`

**C11** - Rendered pipeline-merger prompt requires rebase onto origin/main, `gh pr checks`
green gate, squash merge, and on conflict resolve-or-park with the conflict file list
(RS-04, AC 11)

Proof: `go test ./internal/soul/ -run TestPipelineMergerContract -v`

**C12** - Rendered pipeline-merger prompt states proposal-then-wait (maquinista approve)
as the v1 default before any merge (RS-04, AC 12)

Proof: `go test ./internal/soul/ -run TestPipelineMergerContract -v`

**C13** - All five pipeline templates carry extras keys default_runner and
reasoning_class with pi+standard on worker/fixer/merger and pi+high on reviewer/arbiter
(RS-05, AC 13)

Proof: `go test ./internal/soul/ -run TestPipelineDispatchExtras -v`

**C14** - arch/pipeline.md has a Role souls section documenting the five templates, the
verdict line contract and the extras keys (RS-05, AC 14)

Proof: `rg -c '## Role souls' arch/pipeline.md; echo rc=$?` and `rg -c 'VERDICT: ' arch/pipeline.md; echo rc=$?` and `rg -c 'default_runner' arch/pipeline.md; echo rc=$?`

**C15** - Full gates green: build, vet, gofmt clean on touched files, soul suite green,
spec validators rc=0 (S5)

Proof: `go build ./... && go vet ./...; echo rc=$?` and `gofmt -l internal/soul/pipeline_souls_test.go internal/db/migrations/; echo rc=$?` and `python3 ~/.hermes/skills/tlc-spec-lean/scripts/validate_plan.py role-souls; echo rc=$?` and `python3 ~/.hermes/skills/tlc-spec-lean/scripts/validate_checks.py role-souls; echo rc=$?`

**C16** - Load returns decoded extras for a clone carrying jsonb extras (regression for
the AC 15 amendment: the old []byte assertion silently emptied extras on read-back) —
RS-01, AC 15

Proof: `go test ./internal/soul/ -run TestPipelineTemplateClone -v`

## Known pre-existing failures (out of scope)

- `internal/monitor`: TestOutboxSink_WritesAssistantText, TestOutboxSink_WritesThinking,
  TestToolEventSink_PairedEmitsBoth, TestPiSource_DiscoverBackfill — on record at base
  `f03e05e` and unchanged by EX-01/EX-02 (linear-bridge verification.md). This feature's
  proofs do not run that package.
- `gofmt -l internal/soul/`: compose.go + compose_test.go fail gofmt at base `5b66aee`
  (stash-verified 02/10). Untouched here — C15 scopes gofmt to touched files.

## Swept

- **validation**: C4 · C5 · C2
- **failure modes**: C2 · unknown template clone existing - ErrNotFound path in soul.go
- **idempotency**: C2
- **authorization**: n/a - catalog rows carry no privilege; souls only render into prompts
  of agents the operator or EX-03 dispatch spawns
- **concurrency**: C2 · no new concurrent runtime paths - catalog-only change
- **data lifecycle**: existing - agent_souls.template_id FK RESTRICT (016:37) blocks
  deleting a template that has clones; no retention/rotation surface
- **dependency failure**: n/a - pure SQL seed, no runtime dependency introduced
- **state transitions**: n/a - no tasks/pending_approval/merge_queue code in this feature;
  round accounting stays EX-03/EX-04
- **observability**: existing - migrate runner logs applied migrations; no new logging
  surface

## Coverage

| Set (size) | Member -> proof | Unproven |
| --- | --- | --- |
| pipeline template set (5) | worker C1 · reviewer C1 · arbiter C1 · fixer C1 · merger C1 | - |
| seed idempotency (2) | re-apply no-op C2 · existing rows preserved C2 | - |
| clone fidelity (5 fields) | role C3 · goal C3 · core_truths C3 · boundaries C3 · extras C3 | - |
| extras read seam (1) | clone read-back round trip C16 | - |
| worker contract (3 rules) | spec-first C4 · tools+completion C5 · boundaries C6 | - |
| reviewer/arbiter contract (3 rules) | materials C7 · verdict line C8 · arbiter adjudication C9 | - |
| fixer/merger contract (3 rules) | fixer scope C10 · rebase+gate C11 · proposal-wait C12 | - |
| dispatch hints (10) | worker C13 · reviewer C13 · arbiter C13 · fixer C13 · merger C13 · worker reasoning_class C13 · reviewer reasoning_class C13 · arbiter reasoning_class C13 · fixer reasoning_class C13 · merger reasoning_class C13 | - |
| docs (3 facts) | section exists C14 · verdict contract documented C14 · extras keys documented C14 | - |
| gates (4) | build+vet C15 · gofmt C15 · soul suite C15 · validators C15 | - |
