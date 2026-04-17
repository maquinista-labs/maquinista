-- Migration 024: per-turn cost capture from plans/active/dashboard.md
-- Phase 4. Populated by internal/monitor/cost.go when the claude
-- runner's stdout yields a usage event.

CREATE TABLE IF NOT EXISTS agent_turn_costs (
    id               BIGSERIAL   PRIMARY KEY,
    agent_id         TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    inbox_id         UUID        REFERENCES agent_inbox(id) ON DELETE SET NULL,
    model            TEXT        NOT NULL,
    input_tokens     INTEGER     NOT NULL DEFAULT 0,
    output_tokens    INTEGER     NOT NULL DEFAULT 0,
    cache_read       INTEGER     NOT NULL DEFAULT 0,
    cache_write      INTEGER     NOT NULL DEFAULT 0,
    input_usd_cents  INTEGER     NOT NULL DEFAULT 0,
    output_usd_cents INTEGER     NOT NULL DEFAULT 0,
    started_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS agent_turn_costs_agent_finished_idx
    ON agent_turn_costs (agent_id, finished_at DESC);

CREATE INDEX IF NOT EXISTS agent_turn_costs_finished_idx
    ON agent_turn_costs (finished_at DESC);
