---
name: review-pr
description: Reviewer entry point. Checks out a PR, reviews it, posts feedback.
trigger: /review-pr <pr-number>
---

# /review-pr

The `github-pr-opened` webhook dispatched this pane to you with a PR
number. Your job is to review — not to merge, not to close.

## Loop

1. **Pull the PR locally**:
   ```
   gh pr checkout <pr-number>
   ```
   If that errors, report the error and stop.

2. **Gather context**:
   ```
   gh pr view <pr-number> --json title,body,files,author
   git log main..HEAD --oneline
   git diff main..HEAD --stat
   ```

3. **Read the diff critically**. Focus on:
   - Behavior correctness (does the diff match the PR description?).
   - Test coverage — are the changed lines exercised?
   - Obvious regressions (look at changed exported APIs and their callers).
   - Security / secret leakage (grep for `password`, `token`, `.env` in the diff).

4. **Run tests**: `go test ./...` (or the project's default).

5. **Post the review**:
   - If you'd approve: `gh pr review <pr-number> --approve --body "<summary>"`.
   - If blocking: `gh pr review <pr-number> --request-changes --body "<summary with actionable points>"`.
   - If just comments: `gh pr review <pr-number> --comment --body "<summary>"`.

6. **Stop**. You do not merge. A human or a subsequent workflow closes the
   loop — the `github-pr-merged-or-closed` webhook then calls
   `/close-pr` to let `@pr-closer` update the task state.

## Style

- No nitpicks on formatting — trust the linter.
- Frame blockers as questions ("what happens when X is nil?") rather
  than directives when the ask is ambiguous.
