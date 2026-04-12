---
name: work-on-task
description: Implementor entry point. Implements one task DAG node and opens a PR.
trigger: /work-on-task <task-id>
---

# /work-on-task

You are the implementor for task `<task-id>`. The task-scheduler dispatched
you into this pane; your working directory is already the task's
worktree.

## Loop

1. **Read the spec**. Run:
   ```
   maquinista show <task-id>
   ```
   Absorb the title, body, and any `inherited` or `handoff` context
   entries. Ask the originating topic (via a normal response — §8.2 routes
   it back) only when the spec is genuinely ambiguous.

2. **Work the problem**. Edit, test, iterate in this worktree.

3. **Record observations as you go**:
   ```
   maquinista context add <task-id> observation "<note>"
   ```
   These become `inherited` entries for downstream tasks.

4. **Run tests**:
   - If `metadata.test_cmd` is set on the task, run that.
   - Otherwise, `go test ./...` (or the project's default).

5. **Commit + push** when green:
   ```
   git add <files>
   git commit -m "<task-title>"
   git push -u origin $(git rev-parse --abbrev-ref HEAD)
   ```

6. **Open the PR**:
   ```
   gh pr create --fill
   ```
   Capture the URL from `gh`'s output.

7. **Record the PR URL** so the pipeline advances:
   ```
   maquinista tasks set-pr --id <task-id> --url <pr-url>
   ```
   This flips `tasks.pr_state='open'` and `tasks.status='review'`.

8. **Hand off**:
   ```
   maquinista tasks mark-review <task-id>   # idempotent safety net
   ```
   Your role ends here. The `github-pr-opened` webhook will route the PR
   to `@reviewer`. When the PR merges, `@pr-closer` calls `mark-merged`
   which cascades `refresh_ready_tasks` to unblock dependents.

## If things go wrong

- **Tests failing** you can't fix: `maquinista handoff <task-id> "<why>"`
  and stop. The scheduler's reaper will re-dispatch after your agent
  exits.
- **PR rejected before it opens** (e.g., `gh` errors): record the failure
  in context, do NOT call `set-pr`, and hand off.
