-- Migration 011: task pipeline schema (Appendix D).
-- Adds the columns + indexes required to thread task → agent → PR state
-- through the tasks and agents tables.

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS worktree_path TEXT,
    ADD COLUMN IF NOT EXISTS pr_url        TEXT,
    ADD COLUMN IF NOT EXISTS pr_state      TEXT;

-- Separate constraint so we can drop-and-recreate cleanly if the values
-- widen further. 'review' is a new tasks.status value (widened state
-- machine — see COMMENT below).
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_pr_state_check;
ALTER TABLE tasks
    ADD CONSTRAINT tasks_pr_state_check
    CHECK (pr_state IS NULL OR pr_state IN ('open','merged','closed'));

CREATE INDEX IF NOT EXISTS idx_tasks_pr_url ON tasks(pr_url) WHERE pr_url IS NOT NULL;

-- At most one live (non-dead) agent per task at any moment.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_task_live
    ON agents(task_id)
    WHERE task_id IS NOT NULL AND status != 'dead';

COMMENT ON COLUMN tasks.status IS
    'State machine: pending → ready → claimed → review → done (or failed at any point). '
    'The review state is set when an implementor opens a PR (pr_url populated); MarkMerged '
    'advances review → done, MarkClosed advances review → failed.';
