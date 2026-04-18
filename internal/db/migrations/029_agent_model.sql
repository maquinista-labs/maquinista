-- 029_agent_model.sql
--
-- G.5 of plans/active/dashboard-gaps.md: add a nullable per-agent
-- `model` column so the dashboard spawn-agent modal can persist the
-- operator's model choice. NULL means "use the runtime's default"
-- (today's behaviour for every row).
--
-- The runner LaunchCommand path does not read this column yet —
-- threading it through is a follow-up. For now the dashboard
-- surfaces it as operator metadata and lets the operator override
-- later via a future rename-like dialog.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS model TEXT;
