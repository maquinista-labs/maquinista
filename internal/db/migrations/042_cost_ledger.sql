-- 042_cost_ledger.sql
--
-- MAQ-47: cost-per-task ledger (ADR-0009 F0 DoD "cost-per-task ledger
-- exists" — the F2 pricing-calibration input: tier price = infra cost ×
-- margin × concurrency). No billing — just the rollup.
--
-- Raw data lands in agent_turn_costs (migration 024, monitor/cost.go),
-- model_rates (migration 025) and the turn-end events on agents
-- (last_turn_end_at, migrations 037/041, MAQ-43/44). This migration adds
-- a small session×model rollup table plus the triggers that maintain it,
-- and a "current rates" view for re-pricing tokens at today's model_rates.
--
-- Why a table, not a view over agent_turn_costs: both agent_turn_costs
-- and agent_inbox rows are ON DELETE CASCADE children of agents, and
-- agent rows are per-round mints that get deleted on retire/kill. A
-- view's history would silently shrink; the ledger snapshots user_id and
-- task_id AT TURN TIME and carries no foreign keys, so per-user /
-- per-task totals survive agent deletion.
--
-- Dimensions: per-user (agent_inbox.origin_user_id, falling back to the
-- owner topic_agent_bindings row), per-task (agents.task_id), per-session
-- (one agents row = one session; PK is (agent_id, model) so multi-model
-- sessions roll up cleanly).

CREATE TABLE IF NOT EXISTS cost_ledger (
    agent_id            TEXT        NOT NULL,
    model               TEXT        NOT NULL,
    user_id             TEXT,       -- snapshotted at turn time; survives agent deletion
    task_id             TEXT,       -- snapshotted at turn time
    turns               INTEGER     NOT NULL DEFAULT 0,
    input_tokens        BIGINT      NOT NULL DEFAULT 0,
    output_tokens       BIGINT      NOT NULL DEFAULT 0,
    cache_read_tokens   BIGINT      NOT NULL DEFAULT 0,
    cache_write_tokens  BIGINT      NOT NULL DEFAULT 0,
    captured_cents      BIGINT      NOT NULL DEFAULT 0,  -- insert-time rates (sum of stored usd_cents)
    turn_seconds        DOUBLE PRECISION NOT NULL DEFAULT 0, -- Σ (finished_at - started_at)
    first_turn_at       TIMESTAMPTZ,
    last_turn_at        TIMESTAMPTZ,
    session_started_at  TIMESTAMPTZ,  -- agents.started_at (session wall-clock start)
    turn_end_at         TIMESTAMPTZ,  -- agents.last_turn_end_at (MAQ-43/44 turn-end event)
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (agent_id, model)
);

CREATE INDEX IF NOT EXISTS cost_ledger_task_idx ON cost_ledger (task_id);
CREATE INDEX IF NOT EXISTS cost_ledger_user_idx ON cost_ledger (user_id);
CREATE INDEX IF NOT EXISTS cost_ledger_last_turn_idx ON cost_ledger (last_turn_at DESC);

-- Latest model_rates row per model — the "current rates" the ledger
-- re-prices tokens against (contrast with captured_cents, which froze
-- rates at insert time).
CREATE OR REPLACE VIEW v_model_rates_current AS
SELECT DISTINCT ON (model)
       model,
       input_per_mtok_cents,
       output_per_mtok_cents,
       cache_read_per_mtok_cents,
       cache_write_per_mtok_cents,
       effective_from
FROM model_rates
ORDER BY model, effective_from DESC;

-- --------------------------------------------------------------------
-- Trigger 1: every agent_turn_costs INSERT accumulates into the ledger.
-- User attribution: the turn's inbox row's origin_user_id, else the
-- agent's owner topic binding. Task attribution: agents.task_id.
-- --------------------------------------------------------------------
CREATE OR REPLACE FUNCTION cost_ledger_on_turn() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    snap_user TEXT;
    snap_task TEXT;
BEGIN
    snap_user := COALESCE(
        (SELECT ai.origin_user_id FROM agent_inbox ai WHERE ai.id = NEW.inbox_id),
        (SELECT b.user_id FROM topic_agent_bindings b
          WHERE b.agent_id = NEW.agent_id
            AND b.binding_type = 'owner'
            AND b.user_id IS NOT NULL
          ORDER BY b.created_at
          LIMIT 1)
    );
    snap_task := (SELECT a.task_id FROM agents a WHERE a.id = NEW.agent_id);

    INSERT INTO cost_ledger AS l
        (agent_id, model, user_id, task_id,
         turns, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
         captured_cents, turn_seconds, first_turn_at, last_turn_at, updated_at)
    VALUES
        (NEW.agent_id, NEW.model, snap_user, snap_task,
         1, NEW.input_tokens, NEW.output_tokens, NEW.cache_read, NEW.cache_write,
         NEW.input_usd_cents + NEW.output_usd_cents,
         EXTRACT(EPOCH FROM (NEW.finished_at - NEW.started_at)),
         NEW.started_at, NEW.finished_at, NOW())
    ON CONFLICT (agent_id, model) DO UPDATE SET
        user_id            = COALESCE(l.user_id, EXCLUDED.user_id),
        task_id            = COALESCE(l.task_id, EXCLUDED.task_id),
        turns              = l.turns + 1,
        input_tokens       = l.input_tokens + EXCLUDED.input_tokens,
        output_tokens      = l.output_tokens + EXCLUDED.output_tokens,
        cache_read_tokens  = l.cache_read_tokens + EXCLUDED.cache_read_tokens,
        cache_write_tokens = l.cache_write_tokens + EXCLUDED.cache_write_tokens,
        captured_cents     = l.captured_cents + EXCLUDED.captured_cents,
        turn_seconds       = l.turn_seconds + EXCLUDED.turn_seconds,
        last_turn_at       = EXCLUDED.last_turn_at,
        updated_at         = NOW();
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS cost_ledger_turn ON agent_turn_costs;
CREATE TRIGGER cost_ledger_turn
    AFTER INSERT ON agent_turn_costs
    FOR EACH ROW EXECUTE FUNCTION cost_ledger_on_turn();

-- --------------------------------------------------------------------
-- Trigger 2: session wall-clock. agents.started_at → last_turn_end_at
-- (MAQ-43/44 turn-end event) is the session span; snapshot it onto the
-- ledger while the agents row still exists. UPDATE-only: sessions with
-- no captured turns get no phantom row. Also backfills user/task on
-- ledger rows that were snapshotted before attribution was known.
-- --------------------------------------------------------------------
CREATE OR REPLACE FUNCTION cost_ledger_on_agent() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE cost_ledger SET
        session_started_at = NEW.started_at,
        turn_end_at        = NEW.last_turn_end_at,
        user_id            = COALESCE(cost_ledger.user_id,
            (SELECT b.user_id FROM topic_agent_bindings b
              WHERE b.agent_id = NEW.id
                AND b.binding_type = 'owner'
                AND b.user_id IS NOT NULL
              ORDER BY b.created_at
              LIMIT 1)),
        task_id            = COALESCE(cost_ledger.task_id, NEW.task_id),
        updated_at         = NOW()
    WHERE agent_id = NEW.id;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS cost_ledger_agent_span ON agents;
CREATE TRIGGER cost_ledger_agent_span
    AFTER INSERT OR UPDATE OF started_at, task_id, last_turn_end_at ON agents
    FOR EACH ROW EXECUTE FUNCTION cost_ledger_on_agent();

-- --------------------------------------------------------------------
-- Backfill: roll rows that landed before this migration through the
-- same logic, then snapshot session spans from the surviving agents.
-- Migrations run once; ON CONFLICT DO NOTHING keeps a re-run harmless.
-- --------------------------------------------------------------------
INSERT INTO cost_ledger
    (agent_id, model, user_id, task_id,
     turns, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
     captured_cents, turn_seconds, first_turn_at, last_turn_at, updated_at)
SELECT g.agent_id, g.model,
       (SELECT ai.origin_user_id FROM agent_inbox ai WHERE ai.id = g.any_inbox_id),
       (SELECT a.task_id FROM agents a WHERE a.id = g.agent_id),
       g.turns, g.in_tok, g.out_tok, g.cr_tok, g.cw_tok,
       g.captured, g.secs, g.first_at, g.last_at, NOW()
FROM (
    SELECT atc.agent_id, atc.model,
           COUNT(*)                                                        AS turns,
           SUM(atc.input_tokens)                                           AS in_tok,
           SUM(atc.output_tokens)                                          AS out_tok,
           SUM(atc.cache_read)                                             AS cr_tok,
           SUM(atc.cache_write)                                            AS cw_tok,
           SUM(atc.input_usd_cents + atc.output_usd_cents)                 AS captured,
           SUM(EXTRACT(EPOCH FROM (atc.finished_at - atc.started_at)))     AS secs,
           MIN(atc.started_at)                                             AS first_at,
           MAX(atc.finished_at)                                            AS last_at,
           (SELECT atc2.inbox_id FROM agent_turn_costs atc2
             WHERE atc2.agent_id = atc.agent_id AND atc2.inbox_id IS NOT NULL
             ORDER BY atc2.finished_at DESC LIMIT 1)                       AS any_inbox_id
    FROM agent_turn_costs atc
    GROUP BY atc.agent_id, atc.model
) g
ON CONFLICT (agent_id, model) DO NOTHING;

UPDATE cost_ledger l
   SET session_started_at = a.started_at,
       turn_end_at        = a.last_turn_end_at
  FROM agents a
 WHERE l.agent_id = a.id;

COMMENT ON TABLE cost_ledger IS
'MAQ-47 cost-per-task ledger: session×model rollup of agent_turn_costs + turn-end events (ADR-0009 F0). Trigger-maintained; snapshots user_id/task_id so totals survive agent deletion. No billing.';
