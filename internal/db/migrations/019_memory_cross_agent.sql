-- 019_memory_cross_agent.sql
--
-- Phase 5 of plans/active/agent-memory-db.md: let archival passages be
-- shared beyond a single agent_id. Two new columns:
--
--   owner_scope   — 'agent' (default, the existing per-agent behavior)
--                 | 'project' (scoped to a project id)
--                 | 'global'  (all agents can read)
--   owner_ref     — for 'project' scope carries the project id; NULL
--                 otherwise. Lets multiple agents in the same project
--                 share facts without copying.
--
-- Search helpers (List / Search / FetchForInjection) can opt-in to
-- cross-scope reads via a ListFilter.IncludeShared flag (see
-- internal/memory).

ALTER TABLE agent_memories
    ADD COLUMN IF NOT EXISTS owner_scope TEXT NOT NULL DEFAULT 'agent'
        CHECK (owner_scope IN ('agent','project','global'));
ALTER TABLE agent_memories
    ADD COLUMN IF NOT EXISTS owner_ref TEXT;

CREATE INDEX IF NOT EXISTS agent_memories_scope_idx
    ON agent_memories (owner_scope, owner_ref)
    WHERE owner_scope <> 'agent';
