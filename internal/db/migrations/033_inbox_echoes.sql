-- Migration 033: inbox_echoes — fan-out table for echoing non-Telegram
-- inbox messages (dashboard, future Slack/Discord) back to subscriber
-- channels. Mirrors the channel_deliveries pattern used for agent_outbox
-- so adding a new channel means a new dispatcher, not new plumbing.

-- Track whether an inbox row's echoes have been fanned out.
-- Default FALSE keeps pre-existing rows from being re-processed.
ALTER TABLE agent_inbox
    ADD COLUMN IF NOT EXISTS echo_processed BOOLEAN NOT NULL DEFAULT FALSE;

-- Partial index: only non-Telegram, non-A2A rows that need echo fanout.
CREATE INDEX IF NOT EXISTS idx_agent_inbox_echo_pending
    ON agent_inbox (created_at)
    WHERE NOT echo_processed
      AND origin_channel NOT IN ('telegram', 'a2a');

-- Per-channel delivery record for an inbox row.
CREATE TABLE IF NOT EXISTS inbox_echoes (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    inbox_id        UUID        NOT NULL REFERENCES agent_inbox(id) ON DELETE CASCADE,
    channel         TEXT        NOT NULL DEFAULT 'telegram',
    user_id         TEXT,
    thread_id       TEXT,
    chat_id         BIGINT,
    status          TEXT        NOT NULL DEFAULT 'pending',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_attempt_at TIMESTAMPTZ,
    sent_at         TIMESTAMPTZ,
    external_msg_id BIGINT,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT
);

-- Prevent duplicate echo for the same (inbox row, channel, topic).
CREATE UNIQUE INDEX IF NOT EXISTS uq_inbox_echoes_inbox_channel_thread
    ON inbox_echoes (inbox_id, channel, COALESCE(thread_id, ''));

-- Fast claim path.
CREATE INDEX IF NOT EXISTS idx_inbox_echoes_pending
    ON inbox_echoes (COALESCE(next_attempt_at, created_at))
    WHERE status = 'pending';

-- NOTIFY trigger: wakes the inbox echo dispatcher on each new row.
CREATE OR REPLACE FUNCTION notify_inbox_echo()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM pg_notify('inbox_echo_new', NEW.id::TEXT);
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS on_inbox_echo_notify ON inbox_echoes;
CREATE TRIGGER on_inbox_echo_notify
    AFTER INSERT ON inbox_echoes
    FOR EACH ROW EXECUTE FUNCTION notify_inbox_echo();
