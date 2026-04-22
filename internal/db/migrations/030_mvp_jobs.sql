-- Migration 030: MVP Jobs — nullable agent_id, fresh-agent spawn columns,
-- execution history table.

-- Make agent_id nullable (soul_template_id jobs don't have an agent yet).
ALTER TABLE scheduled_jobs
  ALTER COLUMN agent_id DROP NOT NULL,
  ALTER COLUMN agent_id DROP DEFAULT;

-- New columns for fresh-agent spawning.
ALTER TABLE scheduled_jobs
  ADD COLUMN IF NOT EXISTS soul_template_id  TEXT REFERENCES soul_templates(id),
  ADD COLUMN IF NOT EXISTS context_markdown  TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS agent_cwd         TEXT NOT NULL DEFAULT '';

-- Ensure at least one target is set.
ALTER TABLE scheduled_jobs
  ADD CONSTRAINT chk_job_has_target
    CHECK (agent_id IS NOT NULL OR soul_template_id IS NOT NULL);

-- Execution history — one row per scheduler fire.
CREATE TABLE IF NOT EXISTS job_executions (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id     UUID        NOT NULL REFERENCES scheduled_jobs(id) ON DELETE CASCADE,
  agent_id   TEXT        REFERENCES agents(id) ON DELETE SET NULL,
  started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  ended_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_job_executions_job   ON job_executions(job_id);
CREATE INDEX IF NOT EXISTS idx_job_executions_start ON job_executions(started_at DESC);
