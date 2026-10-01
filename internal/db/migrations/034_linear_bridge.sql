-- Linear ↔ maquinista pipeline bridge (ADR-0005, EX-01): the map row is the
-- bridge's single-owner claim (PK on the Linear issue UUID; UNIQUE task_id),
-- and the sync bookkeeping lives here, not on tasks.

CREATE TABLE linear_issue_map (
  linear_issue_id   TEXT PRIMARY KEY,
  identifier        TEXT NOT NULL,
  team_id           TEXT NOT NULL,
  task_id           TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
  last_synced_state TEXT,
  pending_state     TEXT,
  attempts          INTEGER     NOT NULL DEFAULT 0,
  next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  synced_at         TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Round accounting for the review loop (EX-03/EX-04 consumers).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS review_rounds INTEGER NOT NULL DEFAULT 0;

COMMENT ON TABLE linear_issue_map IS
  'Linear issue ↔ task mirror (ADR-0005): the row is the bridge claim; pending_state is the desired Linear column, last_synced_state the last one successfully pushed.';
