-- MAQ-24: Telegram reply → PR comment exactly-once ledger.
--
-- A non-verb reply to a pipeline notification in the Pipeline topic is
-- posted verbatim as a PR comment (pipeline.PostPRComment). One row per
-- handled Telegram reply; the composite primary key (chat_id, message_id)
-- IS the claim: a retried / redelivered update of the same reply message
-- loses the INSERT race (ON CONFLICT DO NOTHING) and never double-posts.
--
-- disposition audits the outcome:
--   ok      comment posted (comment_url + pr recorded)
--   no_op   clean no-op (task has no open PR — nothing posted, one reply)
--   error   gh/DB failure; the reply stays claimed (a duplicate delivery
--           must not re-post), the operator replies again to retry
-- Rows without a task_id/pr were claimed before the task resolved — never
-- re-processed either way.

CREATE TABLE IF NOT EXISTS telegram_pr_comments (
    chat_id     BIGINT NOT NULL,
    message_id  BIGINT NOT NULL,
    task_id     TEXT,
    pr          INT,
    comment_url TEXT NOT NULL DEFAULT '',
    disposition TEXT NOT NULL DEFAULT 'pending',
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chat_id, message_id)
);

COMMENT ON TABLE telegram_pr_comments IS
  'Consumed Telegram reply message ids (MAQ-24): the INSERT claims a Pipeline-topic reply for exactly-once posting of PR comments; disposition audits the outcome.';
