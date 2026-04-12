-- Migration 009a: deferred-retry column for channel_deliveries.
-- Needed by the Telegram dispatcher (task 1.4): a 429 response reschedules
-- the row by setting status='pending' plus next_attempt_at = NOW() + 30s.
-- The claim predicate skips rows whose defer window has not yet elapsed.
ALTER TABLE channel_deliveries
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_deliveries_ready
    ON channel_deliveries (COALESCE(next_attempt_at, created_at))
    WHERE status = 'pending';
