-- MAQ-11 comment actions: consumed-comment ledger for the ticket-comment
-- approve verb. Provider-neutral: comment_id is the ticket system's comment
-- UUID. A comment recorded here is never re-processed, so a repeated
-- identical approve comment cannot double-merge; the merge_queue partial
-- live index (migration 036) is the second guard on the same invariant.

CREATE TABLE ticket_comment_log (
  comment_id   TEXT PRIMARY KEY,
  task_id      TEXT,
  action       TEXT        NOT NULL DEFAULT 'approve',
  actor        TEXT,
  processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE ticket_comment_log IS
  'Consumed ticket-system comment ids (MAQ-11): the INSERT claims a comment for exactly-once processing of comment verbs (approve).';
