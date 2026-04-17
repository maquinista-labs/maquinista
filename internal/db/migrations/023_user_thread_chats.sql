-- 023_user_thread_chats.sql
--
-- Phase B5 of plans/active/json-state-migration.md: move state.GroupChatIDs
-- (user_id + thread_id → chat_id) out of state.json.

CREATE TABLE IF NOT EXISTS user_thread_chats (
    user_id     TEXT        NOT NULL,
    thread_id   TEXT        NOT NULL,
    chat_id     BIGINT      NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, thread_id)
);
