-- 014_agents_handle.sql
--
-- Adds a nullable user-assigned handle to agents. See
-- plans/per-topic-agent-pivot.md §Q1.
--
-- The stable PK (agents.id) is auto-generated as t-<chat_id>-<thread_id>
-- at tier-3 spawn time and never user-facing. The handle is the
-- friendly alias users type in @mentions and /agent_default. Resolution
-- matches either column (see routing.Resolve).
--
-- Format contract (enforced in application code, not the DB):
--   ^[a-z0-9_-]{2,32}$  with reserved prefix 't-' forbidden so handles
--   cannot shadow auto-ids.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS handle TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_handle_lower
    ON agents (LOWER(handle))
    WHERE handle IS NOT NULL;
