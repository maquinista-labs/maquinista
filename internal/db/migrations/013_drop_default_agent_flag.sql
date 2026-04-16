-- 013_drop_default_agent_flag.sql
--
-- Retires the tier-3 "global default agent" routing from §8.1. The
-- per-topic-agent pivot (plans/archive/per-topic-agent-pivot.md) replaces tier 3
-- with SpawnTopicAgent: each fresh topic spawns its own agent instead of
-- binding to a shared default.
--
-- Forward-only. Existing is_default=TRUE rows simply lose the flag and
-- persist as ordinary agents, reachable via /agent_default @id or
-- @mention.

DROP INDEX IF EXISTS uq_agent_settings_is_default;
ALTER TABLE agent_settings DROP COLUMN IF EXISTS is_default;
