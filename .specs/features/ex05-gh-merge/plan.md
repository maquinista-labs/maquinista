# EX-05 GH merge mode — plan

## Why

EX-04 closed the review loop (verdict → fixer → re-review) but the loop has no
exit: an `approve` verdict parks the task in `ready_to_merge` and a human does
the merge by hand. ADR-0005 req 8–11 define the missing leg — a merger that
rebases onto `origin/main`, waits for CI, squash-merges the GitHub PR, and
cleans up. This plan implements that leg as an opt-in mode so the local
`MergeNoFF` path (migration-002 queue, `cmd_merge.go`) stays untouched for
repos without PR flows.

## Contracts this builds on (frozen, do not re-litigate)

- ADR-0005 req 8: `approve` verdict → merge queue (Linear → Ready to Merge);
  merger rebases onto `origin/main`, squash-merges once CI is green.
- ADR-0005 req 9: rebase conflict → record conflict + task observation
  (queue already records conflicts for local mode, `cmd_merge.go:88-97`);
  unresolvable → Needs Human with the conflict files.
- ADR-0005 req 10: `PIPELINE_AUTO_MERGE=0` (**v1 default**) → the merger does
  not merge on its own; a human unblocks with `maquinista approve <task>`.
- ADR-0005 req 11: merge completes → issue → Done, worktree removed, merge
  note. (The Telegram note is EX-06 plumbing — here we write the observation
  row the note will read.)
- Migration 011 contract: `tasks.pr_url` (indexed) + `tasks.pr_state ∈
  {open, merged, closed}` — the PR is joined to the task by `pr_url`, the
  merge outcome is `pr_state='merged'`.
- Provider seam (EX-01 Rev 2): Done is `SetIssueColumn(ColDone)` through
  `TicketProvider` — no Linear calls from merge code.
- Local `MergeNoFF` + `MergeSquash` + `ConflictError{Files}` exist
  (`internal/git/git.go:113-152`); **no rebase helper exists yet**.

## Design

**Env (pipeline `FromEnv` family):** `PIPELINE_MERGE_MODE=local|gh` (default
`local` — current behavior byte-for-byte), `PIPELINE_AUTO_MERGE=0|1` (default
`0` per req 10). No new env for CI: the gate is part of `gh` mode.

**GH flow** (`internal/pipeline/merge.go`, new; `cmd_merge.go`
`processMerge` branches on mode):

1. `git fetch origin` in the task worktree.
2. `git.Rebase(worktree, "origin/"+entry.BaseBranch)` — new helper, returns
   `*ConflictError{Files}` on conflict.
3. Push the rebased branch: `git push --force-with-lease origin <branch>`.
4. CI gate: PR number parsed from `tasks.pr_url`; `gh pr checks <n>` —
   exit 0 → green; **"no checks reported" → vacuous green** (this repo has no
   workflows; documented policy, mirrors the EX-01 verification note); checks
   pending → retryable (entry released, next tick re-claims); checks failed →
   `task_context` observation `ci_failed` + entry released for retry.
5. `gh pr merge <n> --squash` (no `--delete-branch`: gh's local cleanup dies
   in worktrees — branch deletion is explicit post-cleanup instead).
6. Post-merge: `pr_state='merged'`, `tasks.status='done'`, queue entry
   completed (reuse `db.CompleteMerge`), worktree removed, feature branch
   deleted (local + remote), `SetIssueColumn(ColDone)`, `task_context`
   observation `merged` with the squash SHA (EX-06 renders the Telegram note
   from it).

**Conflict path (v1):** rebase conflict → task `pending_approval` +
observation row carrying the conflict file list (req 9's "record" arm; the
"resolve via merger session" arm needs EX-06 A2A plumbing and is out of
scope — noted, not forgotten).

**AUTO_MERGE=0:** after a `ready_to_merge` landing, the queue entry waits;
`maquinista approve <task-id>` is extended: on a `ready_to_merge` task with
`PIPELINE_MERGE_MODE=gh` it runs the GH flow immediately (approve today only
unblocks `pending_approval`, `cmd_approve.go:15`).

**GhRunner seam:** `type GhRunner interface { PRChecks(ctx, pr int) (state,
error); PRMergeSquash(ctx, pr int) error }` + binary impl; fakes in tests.
Provider is injected the same way sync does it.

## Files

- `internal/git/git.go` + `git_test.go`: `Rebase`, `PushForceWithLease`
  (+ rebase happy/conflict tests on the `initTestRepo` harness).
- `internal/pipeline/merge.go` + `merge_test.go`: mode config, GH flow,
  conflict + CI paths, approve extension hooks (DB-backed, fake GhRunner +
  fake provider).
- `cmd/maquinista/cmd_merge.go`: mode branch (local path untouched).
- `cmd/maquinista/cmd_approve.go`: ready_to_merge arm.
- `internal/pipeline/config.go` (or dispatch config): env parsing.
- `arch/pipeline.md` + `AGENTS.md` package row: merge mode section + env rows.

## Out of scope (explicit)

Telegram proposal/note posting (EX-06), merger-agent spawn on conflict
(EX-06+ A2A), CI authoring for this repo (the gate's vacuous-green policy
covers the no-workflows reality), monorepo paths/retry caps on red CI.
