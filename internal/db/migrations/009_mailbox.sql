-- Migration 009: mailbox schema
-- See plans/maquinista-v2.md §6. Introduces the DB-backed agent mailbox:
-- agent_inbox / agent_outbox / channel_deliveries, conversations,
-- per-(agent, topic) runner sessions, attachments, agent settings, and the
-- NOTIFY triggers that drive the sidecar + relay + dispatcher daemons.

-- --------------------------------------------------------------------
-- Extend topic_agent_bindings (migration 007) with addressing fields.
-- Original PK was (topic_id, agent_id); we drop it and move to a surrogate
-- id so a single (user, thread) can carry multiple bindings (observer rows).
-- --------------------------------------------------------------------
ALTER TABLE topic_agent_bindings
    ADD COLUMN IF NOT EXISTS user_id   TEXT,
    ADD COLUMN IF NOT EXISTS thread_id TEXT,
    ADD COLUMN IF NOT EXISTS chat_id   BIGINT;

-- Backfill legacy rows: treat topic_id (Telegram thread_id) as thread_id.
UPDATE topic_agent_bindings SET thread_id = topic_id::TEXT WHERE thread_id IS NULL;

-- Normalize legacy binding_type default ('observe') to the new vocabulary.
UPDATE topic_agent_bindings SET binding_type = 'observer' WHERE binding_type = 'observe';

ALTER TABLE topic_agent_bindings
    ALTER COLUMN thread_id SET NOT NULL,
    DROP CONSTRAINT IF EXISTS topic_agent_bindings_pkey;

ALTER TABLE topic_agent_bindings
    ADD CONSTRAINT topic_binding_mode_check
        CHECK (binding_type IN ('owner', 'observer'));

-- Preserve legacy uniqueness as a non-unique index for lookup speed.
CREATE INDEX IF NOT EXISTS idx_topic_binding_topic_agent
    ON topic_agent_bindings (topic_id, agent_id);

CREATE UNIQUE INDEX IF NOT EXISTS uq_topic_binding_owner_thread
    ON topic_agent_bindings (user_id, thread_id)
    WHERE binding_type = 'owner';

CREATE INDEX IF NOT EXISTS idx_topic_binding_route
    ON topic_agent_bindings (user_id, thread_id, binding_type);

-- Idempotency for observer rows across reruns of the state.json backfill.
CREATE UNIQUE INDEX IF NOT EXISTS uq_topic_binding_observer
    ON topic_agent_bindings (agent_id, user_id, thread_id)
    WHERE binding_type = 'observer';

-- --------------------------------------------------------------------
-- Per-(agent, topic) runner session IDs.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_topic_sessions (
    agent_id    TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    user_id     TEXT        NOT NULL,
    thread_id   TEXT        NOT NULL,
    runner      TEXT        NOT NULL,
    session_id  TEXT        NOT NULL,
    reset_flag  BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (agent_id, user_id, thread_id)
);

-- --------------------------------------------------------------------
-- Conversations: multi-agent handoff aggregation.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS conversations (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    origin_inbox_id   UUID        NOT NULL,
    origin_user_id    TEXT        NOT NULL,
    origin_thread_id  TEXT        NOT NULL,
    origin_chat_id    BIGINT      NOT NULL,
    pending_count     INT         NOT NULL DEFAULT 1,
    aggregated        BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_conversations_open
    ON conversations(aggregated) WHERE NOT aggregated;

-- --------------------------------------------------------------------
-- Agent inbox.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_inbox (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id         TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    conversation_id  UUID        REFERENCES conversations(id),
    from_kind        TEXT        NOT NULL CHECK (from_kind IN ('user','agent','system')),
    from_id          TEXT,
    origin_channel   TEXT,
    origin_user_id   TEXT,
    origin_thread_id TEXT,
    origin_chat_id   BIGINT,
    external_msg_id  TEXT,
    content          JSONB       NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','processing','processed','failed','dead')),
    claimed_by       TEXT,
    claimed_at       TIMESTAMPTZ,
    lease_expires    TIMESTAMPTZ,
    attempts         INT         NOT NULL DEFAULT 0,
    max_attempts     INT         NOT NULL DEFAULT 5,
    last_error       TEXT,
    enqueued_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at     TIMESTAMPTZ,
    UNIQUE (origin_channel, external_msg_id)
);
CREATE INDEX IF NOT EXISTS idx_inbox_ready
    ON agent_inbox (agent_id, enqueued_at)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_inbox_expired_lease
    ON agent_inbox (lease_expires)
    WHERE status = 'processing';

-- --------------------------------------------------------------------
-- Agent outbox.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_outbox (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    conversation_id UUID        REFERENCES conversations(id),
    in_reply_to     UUID        REFERENCES agent_inbox(id) ON DELETE SET NULL,
    content         JSONB       NOT NULL,
    mentions        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    status          TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','routing','routed','failed')),
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    routed_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending
    ON agent_outbox (created_at)
    WHERE status = 'pending';

-- --------------------------------------------------------------------
-- Channel deliveries: one row per (outbox, subscriber).
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS channel_deliveries (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    outbox_id       UUID        NOT NULL REFERENCES agent_outbox(id) ON DELETE CASCADE,
    channel         TEXT        NOT NULL,
    user_id         TEXT        NOT NULL,
    thread_id       TEXT        NOT NULL,
    chat_id         BIGINT      NOT NULL,
    binding_type    TEXT        NOT NULL CHECK (binding_type IN ('owner','observer','origin')),
    status          TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending','sending','sent','failed','skipped')),
    external_msg_id BIGINT,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ,
    UNIQUE (outbox_id, channel, user_id, thread_id)
);
CREATE INDEX IF NOT EXISTS idx_deliveries_pending
    ON channel_deliveries (created_at)
    WHERE status = 'pending';

-- --------------------------------------------------------------------
-- Attachments in DB.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS message_attachments (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    inbox_id         UUID        REFERENCES agent_inbox(id)  ON DELETE CASCADE,
    outbox_id        UUID        REFERENCES agent_outbox(id) ON DELETE CASCADE,
    name             TEXT        NOT NULL,
    mime_type        TEXT        NOT NULL,
    size_bytes       INT         NOT NULL,
    content          BYTEA,
    large_object_oid OID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((inbox_id IS NOT NULL)::INT + (outbox_id IS NOT NULL)::INT = 1),
    CHECK ((content IS NOT NULL)::INT + (large_object_oid IS NOT NULL)::INT = 1)
);
CREATE INDEX IF NOT EXISTS idx_attachments_inbox
    ON message_attachments(inbox_id)  WHERE inbox_id  IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_attachments_outbox
    ON message_attachments(outbox_id) WHERE outbox_id IS NOT NULL;

-- --------------------------------------------------------------------
-- Agent settings.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS agent_settings (
    agent_id      TEXT        PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    persona       TEXT,
    system_prompt TEXT,
    heartbeat     TEXT,
    roster        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    is_default    BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one global default agent.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_settings_is_default
    ON agent_settings ((1))
    WHERE is_default;

-- --------------------------------------------------------------------
-- NOTIFY triggers.
-- --------------------------------------------------------------------
CREATE OR REPLACE FUNCTION notify_agent_inbox()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' OR (TG_OP = 'UPDATE' AND NEW.status = 'pending' AND OLD.status != 'pending') THEN
        PERFORM pg_notify('agent_inbox_new', NEW.agent_id);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS on_agent_inbox_notify ON agent_inbox;
CREATE TRIGGER on_agent_inbox_notify
    AFTER INSERT OR UPDATE OF status ON agent_inbox
    FOR EACH ROW EXECUTE FUNCTION notify_agent_inbox();

CREATE OR REPLACE FUNCTION notify_agent_outbox()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM pg_notify('agent_outbox_new', NEW.id::TEXT);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS on_agent_outbox_notify ON agent_outbox;
CREATE TRIGGER on_agent_outbox_notify
    AFTER INSERT ON agent_outbox
    FOR EACH ROW EXECUTE FUNCTION notify_agent_outbox();

CREATE OR REPLACE FUNCTION notify_channel_delivery()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM pg_notify('channel_delivery_new', NEW.id::TEXT);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS on_channel_delivery_notify ON channel_deliveries;
CREATE TRIGGER on_channel_delivery_notify
    AFTER INSERT ON channel_deliveries
    FOR EACH ROW EXECUTE FUNCTION notify_channel_delivery();

-- --------------------------------------------------------------------
-- Stop signals (replaces on-disk stop files).
-- --------------------------------------------------------------------
ALTER TABLE agents ADD COLUMN IF NOT EXISTS stop_requested BOOLEAN NOT NULL DEFAULT FALSE;

CREATE OR REPLACE FUNCTION notify_agent_stop()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.stop_requested AND NOT OLD.stop_requested THEN
        PERFORM pg_notify('agent_stop', NEW.id);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS on_agent_stop_notify ON agents;
CREATE TRIGGER on_agent_stop_notify
    AFTER UPDATE OF stop_requested ON agents
    FOR EACH ROW EXECUTE FUNCTION notify_agent_stop();
