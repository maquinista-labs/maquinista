package jobreg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run is one row from the job_runs view.
type Run struct {
	InboxID       string
	FromKind      string
	SourceID      string
	AgentID       string
	EnqueuedAt    time.Time
	ProcessedAt   *time.Time
	Status        string
	LastError     *string
	OutboxID      *string
	AgentResponse []byte
}

// JobRunsByName returns the last `limit` runs for a scheduled_job or
// webhook_handler name, newest first. Paginates with `offset`. Looks up
// the source by name in both tables (scheduled_jobs + webhook_handlers)
// and filters job_runs.source_id accordingly.
func JobRunsByName(ctx context.Context, pool *pgxpool.Pool, name string, limit, offset int) ([]Run, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := pool.Query(ctx, `
		WITH source AS (
			SELECT id::text FROM scheduled_jobs WHERE name=$1
			UNION ALL
			SELECT id::text FROM webhook_handlers WHERE name=$1
		)
		SELECT inbox_id::text, from_kind, source_id, agent_id, enqueued_at,
		       processed_at, status, last_error, outbox_id::text, agent_response
		FROM job_runs
		WHERE source_id IN (SELECT id FROM source)
		ORDER BY enqueued_at DESC
		LIMIT $2 OFFSET $3
	`, name, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.InboxID, &r.FromKind, &r.SourceID, &r.AgentID,
			&r.EnqueuedAt, &r.ProcessedAt, &r.Status, &r.LastError,
			&r.OutboxID, &r.AgentResponse); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Ensure pgx.ErrNoRows stays reachable — retained for callers that want
// to distinguish "no runs yet" from "source doesn't exist."
var _ = pgx.ErrNoRows
