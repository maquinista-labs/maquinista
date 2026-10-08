-- 041_turn_end_freeze_cause.sql
--
-- ADR-0008 F2/F3 (MAQ-44): the turn-end completion contract's scheduler
-- half — one-shot completion nudge + cause-aware freeze ledger.
--
-- The 08/10 incident (MAQ-37 r8): work complete, PR clean, agent ended its
-- turn without `maquinista-done`; the freeze watchdog classified the silent
-- success as a freeze — 30m latency, the last respawn slot burned, and a
-- false needs-human park. Root cause: turn end was not a signal the
-- pipeline could consume, so "finished" and "frozen" were both silence.
--
-- Three columns:
--
--   agents.last_turn_end_at — STICKY signal, written by the monitor
--     (internal/monitor) when the transcript tail shows an assistant
--     message closing the turn with no pending tool call. Never consumed
--     or cleared: the freeze-cause classifier reads it as evidence ("the
--     last observable event of this agent's round was a clean turn end"),
--     so clearing it on nudge would corrupt exactly that evidence. Same
--     family as last_transcript_at (MAQ-9): monitor-written, NULL = never
--     observed.
--
--   agents.turn_end_nudged — the ONE-SHOT guard for the completion nudge.
--     The nudge legs (taskscheduler for the implementor phase,
--     pipeline/dispatch for reviewer/fixer rounds) flip it with a guarded
--     UPDATE (WHERE NOT turn_end_nudged AND status <> 'dead') whose single
--     winner enqueues the nudge — the same exactly-once pattern as the
--     freeze retire's guarded status flip. Agent rows are per-round mints
--     (-rN), so the flag is round-scoped by construction: one nudge per
--     round, ever.
--
--   task_context.cause — the freeze ledger's cause: 'silent_success'
--     (turn end observed before the freeze window elapsed — retires
--     WITHOUT burning respawn budget, straight to review when artifacts
--     allow) or 'true_freeze' (no turn end observed — unchanged behavior,
--     counts against MAQUINISTA_WATCHDOG_RESPAWN_CAP). NULL = written
--     before this migration or by an arm that does not classify (restart
--     cohort sweep, mergers). The respawn-budget counter
--     (pipeline.CountFreezeRetires) counts only 'true_freeze' rows, so
--     pre-migration rows simply stop counting: the ledger semantics change
--     with this release, and a deploy-time budget reset per task+round is
--     bounded and harmless (the systemic-outage loop the cap guards
--     against re-parks within ~2h at the default bounds).

ALTER TABLE agents ADD COLUMN IF NOT EXISTS last_turn_end_at timestamptz;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS turn_end_nudged boolean NOT NULL DEFAULT false;
ALTER TABLE task_context ADD COLUMN IF NOT EXISTS cause text;

COMMENT ON COLUMN agents.last_turn_end_at IS
'ADR-0008: monitor-observed turn end (transcript tail = assistant message, no pending tool call). Sticky evidence for the nudge legs and the freeze-cause classifier; never cleared.';
COMMENT ON COLUMN agents.turn_end_nudged IS
'ADR-0008: the round''s one-shot completion nudge was consumed (guarded UPDATE, one winner). Agent rows are per-round mints, so this caps the nudge at one per round.';
COMMENT ON COLUMN task_context.cause IS
'ADR-0008 freeze-observation cause: silent_success (turn end before the freeze window; no respawn budget burned) or true_freeze (no turn end; counts against MAQUINISTA_WATCHDOG_RESPAWN_CAP). NULL = unclassified (pre-ADR rows, restart sweep, mergers).';
