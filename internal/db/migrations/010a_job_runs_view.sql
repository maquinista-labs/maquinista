-- Migration 010a: job_runs observability view (Appendix C.5).
-- Unifies scheduled + webhook inbox rows with their paired outbox response
-- for simple observability queries. Read-only — all mutations happen on
-- the underlying tables.
CREATE OR REPLACE VIEW job_runs AS
SELECT
    i.id            AS inbox_id,
    i.from_kind,
    i.from_id       AS source_id,
    i.agent_id,
    i.enqueued_at,
    i.processed_at,
    i.status,
    i.last_error,
    o.id            AS outbox_id,
    o.content       AS agent_response
FROM agent_inbox i
LEFT JOIN agent_outbox o ON o.in_reply_to = i.id
WHERE i.from_kind IN ('scheduled','webhook');
