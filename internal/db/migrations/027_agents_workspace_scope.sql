-- 027_agents_workspace_scope.sql
--
-- Phase 3 of plans/active/workspace-scopes.md: give the agents table a
-- first-class notion of its filesystem isolation.
--
-- workspace_scope controls how the reconcile loop resolves the tmux
-- window cwd and (for scope='agent') whether a git worktree is created
-- at <workspace_repo_root>/.maquinista/worktrees/agent/<id>.
--
-- workspace_repo_root is the project's git root. Kept separate from
-- agents.cwd because the agent's cwd becomes the worktree dir once
-- scope='agent' is applied — we still need to remember the parent repo
-- so subsequent restarts can re-derive the layout deterministically.

ALTER TABLE agents
  ADD COLUMN IF NOT EXISTS workspace_scope TEXT NOT NULL DEFAULT 'shared',
  ADD COLUMN IF NOT EXISTS workspace_repo_root TEXT;

-- Defense-in-depth: reject unknown scopes at the DB layer.
ALTER TABLE agents
  DROP CONSTRAINT IF EXISTS agents_workspace_scope_chk;
ALTER TABLE agents
  ADD CONSTRAINT agents_workspace_scope_chk
    CHECK (workspace_scope IN ('shared', 'agent', 'task'));
