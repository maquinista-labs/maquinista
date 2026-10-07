package monitor

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResolveAgentFromWindow returns the agents.id whose tmux_window matches
// the given window id. If the input already matches an agents.id
// directly, return it unchanged (tests / legacy callers pass the real id).
// Returns "" with no error when nothing is found — callers silently skip.
//
// MAQ-38: the resolution must stay deterministic AND live-preferring.
// tmux window ids (@N) restart with the tmux server, so rows from a
// previous daemon epoch can claim the same @N a fresh pane now holds;
// a tie resolved arbitrarily routed outbox rows and transcript-liveness
// touches to a DEAD pre-crash row (observed live 2026-10-07: a dead row
// from the previous day accumulated fresh last_transcript_at while the
// live round starved — the freshness watchdog then false-froze every
// round). The ordering below pins the winner: exact id match, then live
// status, then newest row.
func ResolveAgentFromWindow(ctx context.Context, pool *pgxpool.Pool, windowOrID string) (string, error) {
	return resolveAgentFromWindow(ctx, pool, windowOrID)
}

func resolveAgentFromWindow(ctx context.Context, pool *pgxpool.Pool, windowOrID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE tmux_window = $1 OR id = $1
		ORDER BY (id = $1) DESC,
		         (status IN ('running','working','idle','spawning')) DESC,
		         started_at DESC
		LIMIT 1
	`, windowOrID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}
