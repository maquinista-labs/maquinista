-- EX-06 Telegram plumbing (ADR-0005).
--
-- 1. Synthetic notifier agent: pipeline events are agent_outbox rows written
--    by this row, delivered by the stock relay binding leg to the Pipeline
--    topic (the bot provisioner creates the topic + owner binding). Role and
--    status are chosen so every daemon that scans agents ignores it:
--    reconcile respawns role='user' only; sidecar and monitor select
--    status IN ('running','idle','working'); closeOrphanedTopics reaps
--    'archived'/'dead' only. 'notifier'/'stopped' is touched by none.
-- 2. Live-entry uniqueness: one pending/merging queue entry per task. Closes
--    the enqueue TOCTOU flagged in the EX-05 review; EnqueueMerge pairs it
--    with ON CONFLICT DO NOTHING.
-- 3. Retry accounting for the CI gate: PIPELINE_AUTO_MERGE=1 + a red PR must
--    not release-and-reclaim forever (EX-05 review follow-up).

INSERT INTO agents (id, tmux_session, tmux_window, handle, role, status)
VALUES ('pipeline', 'pipeline', 'pipeline', 'Pipeline', 'notifier', 'stopped')
ON CONFLICT (id) DO NOTHING;

CREATE UNIQUE INDEX IF NOT EXISTS uq_merge_queue_live_task
    ON merge_queue (task_id)
    WHERE status IN ('pending', 'merging');

ALTER TABLE merge_queue ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;
