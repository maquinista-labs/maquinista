# EX-05 GH merge mode — checks

Each check names its proof. House rule: cite `--- PASS` lines + rc, never a
bare exit code. DB-backed proofs use the pipeline testcontainer harness;
git proofs use the `initTestRepo` harness (real temp repos).

- C1 (mode default) `PIPELINE_MERGE_MODE` unset → local mode, `processMerge`
  behavior byte-identical to EX-04 main (source inspection of the branch +
  existing merge-queue tests re-run green unmodified).
  TestMergeMode_DefaultsLocal.
- C2 (rebase helper) `git.Rebase` on the initTestRepo harness: fast-forward
  rebase onto advanced base → success, branch tip parented on new base;
  conflicting change on base → `*ConflictError` with the conflicting file
  list. TestRebase_Success, TestRebase_Conflict.
- C3 (push helper) `git.PushForceWithLease` pushes the rebased tip to the
  remote clone in the harness. TestPushForceWithLease.
- C4 (GH happy path) DB-backed: task `ready_to_merge` + queue entry +
  `pr_url` + fake GhRunner (checks→green, merge→ok) + fake provider →
  run → `pr_state='merged'`, `tasks.status='done'`, entry completed, provider
  saw `SetIssueColumn(ColDone)`, observation row `merged` with squash SHA,
  worktree dir gone, branch deleted. TestMergeGH_HappyPath.
- C5 (conflict path) base moves with a conflicting change → task
  `pending_approval`, observation row lists the conflict files, entry
  resolved-not-merged, provider NOT moved to Done.
  TestMergeGH_RebaseConflict.
- C6 (CI gate) checks pending → entry released, no merge call, task still
  `ready_to_merge`; checks failed → observation `ci_failed`, entry released;
  no-checks-reported → vacuous green → merge proceeds.
  TestMergeGH_CIPendingReleases, TestMergeGH_CIFailObserves,
  TestMergeGH_CINoChecksVacuousGreen.
- C7 (auto-merge default) `PIPELINE_AUTO_MERGE` unset → after C4-style setup,
  the loop does NOT merge: entry stays queued, task stays
  `ready_to_merge`, PRMerge never called. TestMergeGH_AutoMergeOffWaits.
- C8 (approve unblock) `maquinista approve <task>` on a `ready_to_merge`
  task with mode gh → runs the GH flow (same end-state as C4); approve on
  `pending_approval` still works (regression). TestApprove_ReadyToMergeRunsGH,
  TestApprove_PendingApprovalRegression.
- C9 (neutrality + cleanliness) `rg -n -i 'linear' internal/pipeline/merge*.go
  cmd/maquinista/cmd_merge.go` → zero hits; provider accessed only via the
  injected seam; `go build ./...` + `go vet ./internal/... ./cmd/...` green;
  `git diff base..HEAD` touches no migrations and no soul templates.
- C10 (docs) arch/pipeline.md `## Merge mode (EX-05)` naming the flow + env
  table rows `PIPELINE_MERGE_MODE` / `PIPELINE_AUTO_MERGE`; AGENTS.md package
  row mentions merge mode; AGENTS.md move complete (CLAUDE.md is a stub
  importing @AGENTS.md).

## Full-suite gate

`go test ./internal/pipeline/... ./internal/db/... ./internal/git/...
./internal/agentspawn/...` with named PASS lines. Merge-queue local-mode
tests re-run unmodified (C1's regression arm). Known-failing-at-base monitor
tests (outbox/tool-event trio) remain excluded — outside gate packages.
