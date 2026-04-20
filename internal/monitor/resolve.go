package monitor

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resolveAgentFromWindow returns the agents.id whose tmux_window matches
// the given window id. If the input already matches an agents.id
// directly, return it unchanged (tests / legacy callers pass the real id).
// Returns "" with no error when nothing is found — callers silently skip.
func resolveAgentFromWindow(ctx context.Context, pool *pgxpool.Pool, windowOrID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE tmux_window = $1 OR id = $1
		ORDER BY (id = $1) DESC
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
