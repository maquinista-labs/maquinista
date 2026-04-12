---
name: close-pr
description: Advance tasks.pr_state based on a github PR close/merge event.
trigger: /close-pr <pr-number> <action>
---

# /close-pr

The `github-pr-merged-or-closed` webhook dispatched this pane. The
webhook's prompt_template rendered two args for you:

- `<pr-number>` — the PR that closed/merged.
- `<action>` — one of `merged` or `closed`.

You are `@pr-closer`. Flip the task state and stop.

## Loop

1. **Look up the task** by PR URL. The webhook payload's `pull_request.html_url`
   is the most reliable identifier; reconstruct it if needed.
   ```
   maquinista tasks by-pr --url https://github.com/<owner>/<repo>/pull/<pr-number>
   ```

2. **Flip state**:
   - If `<action>` is `merged`:
     ```
     maquinista tasks mark-merged <task-id>
     ```
     This sets `tasks.pr_state='merged'`, `tasks.status='done'`,
     `tasks.done_at=NOW()`. The `refresh_ready_tasks` trigger promotes
     eligible `pending` dependents to `ready` — no further action needed.
   - If `<action>` is `closed` (i.e., rejected or abandoned):
     ```
     maquinista tasks mark-closed <task-id>
     ```
     This sets `pr_state='closed'`, `status='failed'`. Dependents stay
     blocked.

3. **Confirm** with a short summary:
   ```
   maquinista show <task-id>
   ```
   Quote the row's final state (status, pr_state, done_at).

4. **Stop**. Do not spawn further agents; the task-scheduler picks up
   whatever new `ready` tasks the trigger promoted.

## Edge cases

- **No task for this PR URL**: post a note in the originating topic and
  stop. This usually means the PR was created outside the pipeline.
- **Task already in `status='done'`**: `mark-merged` is idempotent —
  re-running is harmless.
