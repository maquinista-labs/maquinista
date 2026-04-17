package state

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionMapEntry carries the per-tmux-window runner-session metadata
// that was previously stored in session_map.json. Phase A of
// plans/active/json-state-migration.md moved the source of truth to the
// agents table (migration 015 added session_id/cwd/window_name/started_at
// columns). This struct is now a pure projection of those columns.
type SessionMapEntry struct {
	SessionID       string
	CWD             string
	WindowName      string
	WindowCreatedAt int64 // Unix ms; derived from agents.started_at
}

// LoadSessionMap returns the live session_map keyed by
// "<tmux_session>:<tmux_window>". Reads from Postgres exclusively — there
// is no on-disk session_map.json anymore. A nil pool yields an empty map
// (caller then has nothing to route / clean up).
func LoadSessionMap(ctx context.Context, pool *pgxpool.Pool) (map[string]SessionMapEntry, error) {
	out := map[string]SessionMapEntry{}
	if pool == nil {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT tmux_session, tmux_window,
		       COALESCE(session_id,''), COALESCE(cwd,''), COALESCE(window_name,''),
		       COALESCE(EXTRACT(EPOCH FROM started_at)*1000, 0)::bigint
		FROM agents
		WHERE status IN ('running','working','idle')
		  AND tmux_session <> '' AND tmux_window <> ''
	`)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("load session map: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tmuxSession, tmuxWindow, sessionID, cwd, windowName string
		var startedAtMS int64
		if err := rows.Scan(&tmuxSession, &tmuxWindow, &sessionID, &cwd, &windowName, &startedAtMS); err != nil {
			return nil, err
		}
		out[tmuxSession+":"+tmuxWindow] = SessionMapEntry{
			SessionID:       sessionID,
			CWD:             cwd,
			WindowName:      windowName,
			WindowCreatedAt: startedAtMS,
		}
	}
	return out, rows.Err()
}

// LookupSessionByWindow returns the session metadata for a single
// (tmux_session, tmux_window) pair. Equivalent to reading one entry from
// the old session_map.json.
func LookupSessionByWindow(ctx context.Context, pool *pgxpool.Pool, tmuxSession, tmuxWindow string) (SessionMapEntry, bool, error) {
	if pool == nil {
		return SessionMapEntry{}, false, nil
	}
	var e SessionMapEntry
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(session_id,''), COALESCE(cwd,''), COALESCE(window_name,''),
		       COALESCE(EXTRACT(EPOCH FROM started_at)*1000, 0)::bigint
		FROM agents
		WHERE tmux_session = $1 AND tmux_window = $2
	`, tmuxSession, tmuxWindow).Scan(&e.SessionID, &e.CWD, &e.WindowName, &e.WindowCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionMapEntry{}, false, nil
	}
	if err != nil {
		return SessionMapEntry{}, false, fmt.Errorf("lookup session: %w", err)
	}
	return e, true, nil
}
