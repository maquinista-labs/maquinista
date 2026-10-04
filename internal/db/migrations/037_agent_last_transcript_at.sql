-- MAQ-9 transcript liveness: a per-agent signal that the session transcript
-- GREW recently, distinct from agent_outbox activity. The monitor tails the
-- runner's JSONL and advances its read offset through tool events; the
-- pipeline watchdog (`internal/pipeline/dispatch.go`) uses this column to
-- avoid parking healthy reviewers/fixers that are mid-command (a long
-- `go test ./...` streams tool events but writes zero outbox rows).
--
-- NULL = no transcript growth observed since spawn (the transcript file did
-- not exist yet, or the monitor never saw an offset advance). The watchdog
-- treats NULL as "no transcript activity" — the young-agent age guard
-- (started_at < NOW() - timeout) still protects newborns.
--
-- Written throttled (≥30s per window) by internal/monitor on offset advance.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS last_transcript_at timestamptz;
