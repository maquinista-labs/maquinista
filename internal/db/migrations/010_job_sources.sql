-- Migration 010: programmatic job sources (scheduled + webhook) from
-- plans/reference/maquinista-v2.md Appendix C. Both tables feed agent_inbox rows
-- — we also widen the from_kind check so inbox consumers accept them.

CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT        NOT NULL UNIQUE,
    cron_expr         TEXT        NOT NULL,
    timezone          TEXT        NOT NULL DEFAULT 'UTC',
    agent_id          TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    prompt            JSONB       NOT NULL,
    reply_channel     JSONB,
    warm_spawn_before INTERVAL,
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    next_run_at       TIMESTAMPTZ NOT NULL,
    last_run_at       TIMESTAMPTZ,
    last_inbox_id     UUID        REFERENCES agent_inbox(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_scheduled_due
    ON scheduled_jobs (next_run_at) WHERE enabled;

CREATE TABLE IF NOT EXISTS webhook_handlers (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT        NOT NULL UNIQUE,
    path               TEXT        NOT NULL,
    secret             TEXT        NOT NULL,
    signature_scheme   TEXT        NOT NULL DEFAULT 'github-hmac-sha256',
    event_filter       JSONB,
    agent_id           TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    prompt_template    TEXT        NOT NULL,
    reply_channel      JSONB,
    rate_limit_per_min INT         NOT NULL DEFAULT 60,
    enabled            BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Partial unique index: only one enabled handler can own a given path.
CREATE UNIQUE INDEX IF NOT EXISTS uq_webhook_path_enabled
    ON webhook_handlers (path) WHERE enabled;

-- Widen agent_inbox.from_kind to accept the new programmatic sources.
ALTER TABLE agent_inbox DROP CONSTRAINT IF EXISTS agent_inbox_from_kind_check;
ALTER TABLE agent_inbox
    ADD CONSTRAINT agent_inbox_from_kind_check
    CHECK (from_kind IN ('user','agent','system','scheduled','webhook'));
