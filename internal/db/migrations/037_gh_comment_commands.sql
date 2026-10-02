-- MAQ-12: GitHub comment commands ("maquinista <verb>" on PR conversations).
--
-- One row per processed PR conversation comment. The primary key is the
-- globally-unique GitHub comment id, so the INSERT (ON CONFLICT DO NOTHING)
-- IS the exactly-once claim: a duplicate command comment never re-runs its
-- verb — a repeated `maquinista approve` can never double-merge (the
-- merge_queue live-entry index is the second guard, this table is the
-- comment-memory the dispatcher skips re-parsing on).
--
-- disposition records what the dispatcher did, for audit:
--   pending       claimed, handler not finished yet
--   ok            verb handler ran successfully
--   no_op         clean single no-op (no task for the PR, task not in a
--                 state the verb applies to, unknown verb)
--   unauthorized  commenter not in the allowed-logins set (ignored silently)
--   error         handler failed; the next identical command may retry
-- Rows are also how re-polling stays cheap: a comment already claimed here
-- is skipped before any handler or gh call runs.

CREATE TABLE IF NOT EXISTS gh_comment_commands (
    comment_id   BIGINT PRIMARY KEY,
    verb         TEXT        NOT NULL,
    task_id      TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    actor        TEXT        NOT NULL DEFAULT '',
    disposition  TEXT        NOT NULL DEFAULT 'pending',
    detail       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
