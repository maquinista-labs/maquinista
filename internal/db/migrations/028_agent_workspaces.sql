-- 028_agent_workspaces.sql
--
-- Phase 6 of plans/active/workspace-scopes.md: promote workspaces to
-- first-class child rows so an agent can hold N workspaces and switch
-- between them (Hermes "identity survives, workspace rotates" model,
-- but with human-chosen labels instead of session timestamps).
--
-- Data model:
--   agent_workspaces(id, agent_id, scope, repo_root, worktree_dir,
--                   branch, created_at, archived_at)
--   agents.active_workspace_id → FK to agent_workspaces(id)
--
-- The existing agents.workspace_scope + workspace_repo_root columns
-- (from migration 027) stay as a denormalized cache of the active
-- workspace, kept synced by a BEFORE-UPDATE trigger so the reconcile
-- loop doesn't have to join on every pass.

CREATE TABLE IF NOT EXISTS agent_workspaces (
    id            TEXT        PRIMARY KEY,
    agent_id      TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    scope         TEXT        NOT NULL,
    repo_root     TEXT        NOT NULL,
    worktree_dir  TEXT,
    branch        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at   TIMESTAMPTZ,
    CONSTRAINT agent_workspaces_scope_chk CHECK (scope IN ('shared', 'agent', 'task')),
    CONSTRAINT agent_workspaces_worktree_scope_chk CHECK (
        (scope = 'shared' AND worktree_dir IS NULL AND branch IS NULL) OR
        (scope IN ('agent', 'task') AND worktree_dir IS NOT NULL AND branch IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_agent_workspaces_agent
    ON agent_workspaces(agent_id, archived_at);

ALTER TABLE agents
    ADD COLUMN IF NOT EXISTS active_workspace_id TEXT REFERENCES agent_workspaces(id);

-- Backfill: every existing agent row gets a companion workspace
-- "<agent_id>@default" derived from its current scope + repo_root
-- (or cwd / empty for scope=shared without a repo_root).
--
-- Compute worktree_dir + branch deterministically to match
-- internal/agent/workspace.go ResolveLayout(). Keep in sync with
-- that function — the SQL formula is the canonical runtime formula.
INSERT INTO agent_workspaces (id, agent_id, scope, repo_root, worktree_dir, branch)
SELECT
    a.id || '@default'                                           AS id,
    a.id                                                          AS agent_id,
    COALESCE(NULLIF(a.workspace_scope, ''), 'shared')             AS scope,
    COALESCE(
        NULLIF(a.workspace_repo_root, ''),
        NULLIF(a.cwd, ''),
        ''
    )                                                             AS repo_root,
    CASE
        WHEN COALESCE(a.workspace_scope, 'shared') IN ('agent', 'task')
        THEN COALESCE(NULLIF(a.workspace_repo_root, ''), NULLIF(a.cwd, ''), '')
             || '/.maquinista/worktrees/'
             || COALESCE(a.workspace_scope, 'agent')
             || '/'
             || a.id
        ELSE NULL
    END                                                           AS worktree_dir,
    CASE
        WHEN COALESCE(a.workspace_scope, 'shared') IN ('agent', 'task')
        THEN 'maquinista/' || COALESCE(a.workspace_scope, 'agent') || '/' || a.id
        ELSE NULL
    END                                                           AS branch
FROM agents a
WHERE NOT EXISTS (
    SELECT 1 FROM agent_workspaces w WHERE w.id = a.id || '@default'
)
AND (
    -- Only backfill when the scope is well-formed. scope=agent/task
    -- rows missing a repo_root (workspace_repo_root empty AND cwd empty)
    -- are skipped — they would violate the worktree_scope_chk constraint
    -- and likely indicate a bug. Operator can create a workspace by hand.
    COALESCE(a.workspace_scope, 'shared') = 'shared'
    OR COALESCE(NULLIF(a.workspace_repo_root, ''), NULLIF(a.cwd, ''), '') <> ''
);

-- Point each agent at its freshly-backfilled workspace.
UPDATE agents
SET active_workspace_id = id || '@default'
WHERE active_workspace_id IS NULL
  AND EXISTS (
    SELECT 1 FROM agent_workspaces w WHERE w.id = agents.id || '@default'
  );

-- Trigger: whenever active_workspace_id changes, mirror the active
-- row's scope + repo_root into agents.workspace_scope +
-- workspace_repo_root so the denormalized reconcile-hot-path columns
-- stay truthful.
CREATE OR REPLACE FUNCTION sync_active_workspace()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    ws_scope TEXT;
    ws_repo  TEXT;
BEGIN
    IF NEW.active_workspace_id IS NOT NULL THEN
        SELECT scope, repo_root INTO ws_scope, ws_repo
        FROM agent_workspaces
        WHERE id = NEW.active_workspace_id;
        IF ws_scope IS NOT NULL THEN
            NEW.workspace_scope     := ws_scope;
            NEW.workspace_repo_root := NULLIF(ws_repo, '');
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS agents_sync_active_workspace ON agents;
CREATE TRIGGER agents_sync_active_workspace
BEFORE UPDATE OF active_workspace_id ON agents
FOR EACH ROW
EXECUTE FUNCTION sync_active_workspace();
