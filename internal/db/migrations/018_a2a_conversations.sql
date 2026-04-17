-- 018_a2a_conversations.sql
--
-- Phase 2 of plans/active/agent-to-agent-communication.md: promote
-- conversations from "one per human-originated inbox row" to a
-- first-class handoff-chain container. Adds:
--
--   kind                   — 'external' | 'a2a' | 'broadcast' | 'system'
--   participants           — the set of agent ids involved
--   topic                  — optional short label / summary
--   parent_conversation_id — for nested sub-conversations
--
-- GIN index over participants so "open conversations involving X" is a
-- cheap query (the relay uses it to re-use an existing a2a conversation
-- instead of creating a new one for every mention).

ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'external'
        CHECK (kind IN ('external','a2a','broadcast','system'));
ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS participants TEXT[] NOT NULL DEFAULT '{}'::text[];
ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS topic TEXT;
ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS parent_conversation_id UUID REFERENCES conversations(id);

CREATE INDEX IF NOT EXISTS conversations_participants_idx
    ON conversations USING GIN (participants);

-- Relax the origin_* NOT NULL constraints so a2a / system / broadcast
-- conversations (which have no Telegram origin) can be inserted.
ALTER TABLE conversations
    ALTER COLUMN origin_inbox_id DROP NOT NULL;
ALTER TABLE conversations
    ALTER COLUMN origin_user_id DROP NOT NULL;
ALTER TABLE conversations
    ALTER COLUMN origin_thread_id DROP NOT NULL;
ALTER TABLE conversations
    ALTER COLUMN origin_chat_id DROP NOT NULL;
