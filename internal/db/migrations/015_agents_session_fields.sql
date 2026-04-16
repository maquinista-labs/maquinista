-- 015_agents_session_fields.sql
--
-- Phase A of plans/active/json-state-migration.md: move the per-agent
-- runner-session metadata off session_map.json and onto columns on
-- agents. Once the monitor sources read from these columns the JSON file
-- is retired.
--
-- All nullable — the tier-4 picker path (and a freshly-spawned agent
-- that hasn't yet created a runner session) leaves them empty. session_id
-- is Claude's UUID at SessionStart, OpenCode's session id from its
-- internal DB, or the agent_id itself for runners without a session
-- concept.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS session_id  TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS cwd         TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS window_name TEXT;
