-- Ticket system ↔ maquinista pipeline bridge (ADR-0005 + ADR-0006, EX-01):
-- the map row is the bridge's single-owner claim (PK on the provider issue
-- ID; UNIQUE task_id), and the sync bookkeeping lives here, not on tasks.
-- Provider-neutral: issue_id is the ticket system's issue UUID, issue_key the
-- human key (e.g. MAQ-2), team_id the board/team identifier within the
-- provider. pending_state/last_synced_state store canonical column names
-- (provider.go Column.String()).

CREATE TABLE ticket_issue_map (
  issue_id          TEXT PRIMARY KEY,
  issue_key         TEXT NOT NULL,
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

COMMENT ON TABLE ticket_issue_map IS
  'Ticket issue ↔ task mirror (ADR-0005/0006): the row is the bridge claim; pending_state is the desired canonical column, last_synced_state the last one successfully pushed.';
