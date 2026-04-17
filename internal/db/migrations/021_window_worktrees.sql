-- 021_window_worktrees.sql
--
-- Phase B3 of plans/active/json-state-migration.md: move state.WorktreeBindings
-- (thread_id → {WorktreeDir, Branch, RepoRoot, BaseBranch, TaskID, IsMergeTopic})
-- out of state.json and onto a DB table keyed by thread_id.

CREATE TABLE IF NOT EXISTS window_worktrees (
    thread_id      TEXT        PRIMARY KEY,
    worktree_dir   TEXT        NOT NULL,
    branch         TEXT        NOT NULL,
    repo_root      TEXT        NOT NULL,
    base_branch    TEXT        NOT NULL DEFAULT '',
    task_id        TEXT,
    is_merge_topic BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
