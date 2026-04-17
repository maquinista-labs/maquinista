-- Migration 025: model_rates lookup. plans/active/dashboard.md Phase 4.
-- Rates are in USD per million tokens, expressed in micro-cents
-- (1 cent / 10_000 tokens precision = 100 µ¢ per 1k tokens).
-- Seed values are conservative 2025-2026 ballpark — the cost
-- capture computes usd_cents from these rates AT INSERT TIME so
-- historical rows stay accurate when prices change.

CREATE TABLE IF NOT EXISTS model_rates (
    id                    BIGSERIAL   PRIMARY KEY,
    model                 TEXT        NOT NULL,
    input_per_mtok_cents  INTEGER     NOT NULL,
    output_per_mtok_cents INTEGER     NOT NULL,
    cache_read_per_mtok_cents   INTEGER NOT NULL DEFAULT 0,
    cache_write_per_mtok_cents  INTEGER NOT NULL DEFAULT 0,
    effective_from        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model, effective_from)
);

-- Seed rows. Ballpark values; operators can insert their own with
-- a newer effective_from and capture picks the latest row at or
-- before finished_at.
INSERT INTO model_rates
  (model, input_per_mtok_cents, output_per_mtok_cents,
   cache_read_per_mtok_cents, cache_write_per_mtok_cents,
   effective_from)
VALUES
  ('claude-sonnet-4-6',   300,  1500,  30,  375, '2025-01-01'),
  ('claude-opus-4-6',     1500, 7500,  150, 1875, '2025-01-01'),
  ('claude-haiku-4-6',    80,   400,   8,   100, '2025-01-01')
ON CONFLICT (model, effective_from) DO NOTHING;
