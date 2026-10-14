// Package db — cost ledger rollups (MAQ-47, ADR-0009 F0).
//
// cost_ledger is trigger-maintained (migration 042): every
// agent_turn_costs INSERT accumulates into a session×model row with
// user_id/task_id snapshotted at turn time, and agents
// started/last_turn_end_at updates refresh the session wall-clock span.
// This file is read-side only: one rollup query the `maquinista ledger`
// verb groups per session, per task, or per user.
//
// Costs come in two flavors:
//   - CapturedCents — the usd_cents frozen at insert time from the
//     model_rates row then in effect (monitor/cost.go).
//   - CurrentCents  — tokens re-priced against v_model_rates_current.
//     NULL when any row in the group has no rate for its model: a
//     partial sum would silently understate.

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LedgerBy selects the rollup grain for LedgerRollup.
type LedgerBy string

const (
	LedgerBySession LedgerBy = "session" // one row per agent session
	LedgerByTask    LedgerBy = "task"    // one row per task (NULL → "(interactive)")
	LedgerByUser    LedgerBy = "user"    // one row per user (NULL → "(unattributed)")
)

// Sentinels for ledger rows whose user/task snapshot is still unknown
// (e.g. pipeline rounds with no inbox row and no binding).
const (
	LedgerNoUser      = "(unattributed)"
	LedgerInteractive = "(interactive)"
)

// LedgerRow is one rollup line: a group key plus summed usage metrics.
type LedgerRow struct {
	// Session grain only; nil on task/user grain.
	AgentID *string `json:"agent_id,omitempty"`
	UserID  *string `json:"user_id,omitempty"`
	TaskID  *string `json:"task_id,omitempty"`

	// Key is the display label: agent id, task id (or "(interactive)"),
	// or user id (or "(unattributed)").
	Key string `json:"key"`

	Turns            int64 `json:"turns"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`

	// CapturedCents sums the insert-time usd_cents. CurrentCents is
	// nil when the group mixes in a model with no current rate.
	CapturedCents int64  `json:"captured_cents"`
	CurrentCents  *int64 `json:"current_cents,omitempty"`

	// TurnSeconds is Σ(turn wall-clock). WallSeconds is the session
	// span (started_at → last_turn_end_at), max over the group; nil
	// when no turn-end event was observed.
	TurnSeconds float64  `json:"turn_seconds"`
	WallSeconds *float64 `json:"wall_seconds,omitempty"`

	FirstTurnAt *time.Time `json:"first_turn_at,omitempty"`
	LastTurnAt  *time.Time `json:"last_turn_at,omitempty"`
}

// LedgerRollup groups cost_ledger by the requested grain. Filters are
// ANDed and optional; since bounds on last_turn_at (session activity).
// Rows order by cost (current when fully priced, else captured) desc.
func LedgerRollup(ctx context.Context, pool *pgxpool.Pool, by LedgerBy,
	userID, taskID *string, since *time.Time, limit int,
) ([]LedgerRow, error) {
	if pool == nil {
		return nil, fmt.Errorf("db: LedgerRollup requires a pool")
	}
	if limit <= 0 {
		limit = 25
	}

	// Key expression, id columns, and GROUP BY per grain. Session
	// grain groups by the raw columns (stable per-agent snapshots, so
	// the grouping is exact); on coarser grains the id columns are
	// either the key itself or informational aggregates.
	var keyExpr, idCols, groupBy string
	switch by {
	case LedgerBySession:
		keyExpr = "agent_id"
		idCols = "agent_id, user_id, task_id"
		groupBy = "agent_id, user_id, task_id"
	case LedgerByTask:
		keyExpr = fmt.Sprintf("COALESCE(task_id, '%s')", LedgerInteractive)
		idCols = "NULL::text, MIN(user_id), NULL::text"
		groupBy = keyExpr
	case LedgerByUser:
		keyExpr = fmt.Sprintf("COALESCE(user_id, '%s')", LedgerNoUser)
		idCols = "NULL::text, NULL::text, MIN(task_id)"
		groupBy = keyExpr
	default:
		return nil, fmt.Errorf("db: unknown ledger grain %q", by)
	}

	where, args := "TRUE", []any{}
	if userID != nil {
		args = append(args, *userID)
		where += fmt.Sprintf(" AND user_id = $%d", len(args))
	}
	if taskID != nil {
		args = append(args, *taskID)
		where += fmt.Sprintf(" AND task_id = $%d", len(args))
	}
	if since != nil {
		args = append(args, *since)
		where += fmt.Sprintf(" AND last_turn_at >= $%d", len(args))
	}
	args = append(args, limit)
	limitPh := fmt.Sprintf("$%d", len(args))

	// priced: per (session, model) current-rate cost + session span in
	// one pass. Integer division by 1e6 mirrors monitor/cost.go.
	// Group-level CurrentCents is NULL unless every row priced.
	sql := fmt.Sprintf(`
		WITH priced AS (
			SELECT l.*,
			       CASE WHEN r.model IS NULL THEN NULL ELSE
			           (l.input_tokens       * r.input_per_mtok_cents
			            + l.cache_read_tokens  * r.cache_read_per_mtok_cents) / 1000000
			         + (l.output_tokens        * r.output_per_mtok_cents
			            + l.cache_write_tokens * r.cache_write_per_mtok_cents) / 1000000
			       END AS current_cents,
			       CASE WHEN l.turn_end_at IS NOT NULL
			             AND l.session_started_at IS NOT NULL THEN
			           EXTRACT(EPOCH FROM (l.turn_end_at - l.session_started_at))
			       END AS wall_seconds
			FROM cost_ledger l
			LEFT JOIN v_model_rates_current r ON r.model = l.model
			WHERE %s
		)
		SELECT %s AS key,
		       %s,
		       SUM(turns)                       AS turns,
		       SUM(input_tokens)                AS in_tok,
		       SUM(output_tokens)               AS out_tok,
		       SUM(cache_read_tokens)           AS cr_tok,
		       SUM(cache_write_tokens)          AS cw_tok,
		       SUM(captured_cents)              AS captured,
		       CASE WHEN BOOL_AND(current_cents IS NOT NULL)
		            THEN SUM(current_cents) END AS current,
		       SUM(turn_seconds)                AS turn_secs,
		       MAX(wall_seconds)                AS wall_secs,
		       MIN(first_turn_at)               AS first_at,
		       MAX(last_turn_at)                AS last_at
		FROM priced
		GROUP BY %s
		ORDER BY COALESCE(SUM(current_cents), SUM(captured_cents)) DESC,
		         MAX(last_turn_at) DESC NULLS LAST
		LIMIT %s
	`, where, keyExpr, idCols, groupBy, limitPh)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("db: ledger rollup: %w", err)
	}
	defer rows.Close()

	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(
			&r.Key, &r.AgentID, &r.UserID, &r.TaskID,
			&r.Turns, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens,
			&r.CapturedCents, &r.CurrentCents,
			&r.TurnSeconds, &r.WallSeconds,
			&r.FirstTurnAt, &r.LastTurnAt,
		); err != nil {
			return nil, fmt.Errorf("db: ledger rollup scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
