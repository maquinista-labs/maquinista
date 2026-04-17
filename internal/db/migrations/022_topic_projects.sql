-- 022_topic_projects.sql
--
-- Phase B4 of plans/active/json-state-migration.md: move state.ProjectBindings
-- (thread_id → project_id) out of state.json.

CREATE TABLE IF NOT EXISTS topic_projects (
    thread_id   TEXT        PRIMARY KEY,
    project_id  TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
