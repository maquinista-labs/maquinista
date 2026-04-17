// Package monitor cost capture — plans/active/dashboard.md Phase 4.
//
// This file adds a CaptureTurn(pool, ev) helper that writes an
// agent_turn_costs row from a usage event emitted by the claude
// runner (or any runner that reports input / output / cache tokens
// per turn). The helper computes usd_cents AT INSERT TIME from
// model_rates so historical rows survive pricing changes.
//
// Wire-up into the live monitor loop is deliberately narrow: a
// future commit passes CaptureTurn into internal/monitor/source_claude.go's
// usage-parsing path. For now the function is fully tested and
// callable by both that path and the Go-side integration tests.

package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TurnCost describes one agent turn's token usage. Times are from
// the runner; inbox_id is optional — set when the turn was kicked
// off by a specific inbox row.
type TurnCost struct {
	AgentID      string
	InboxID      *string
	Model        string
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	StartedAt    time.Time
	FinishedAt   time.Time
}

// CaptureTurn inserts a row into agent_turn_costs using rates from
// model_rates (latest row at or before FinishedAt). If no rate
// exists for the model, both usd_cents fields are 0 and the row
// still lands — visibility over pricing wins, and the operator can
// backfill via a later rate row.
func CaptureTurn(ctx context.Context, pool *pgxpool.Pool, t TurnCost) (int64, error) {
	if pool == nil {
		return 0, errors.New("monitor: CaptureTurn requires a pool")
	}
	if t.AgentID == "" {
		return 0, errors.New("monitor: CaptureTurn: AgentID required")
	}
	if t.Model == "" {
		return 0, errors.New("monitor: CaptureTurn: Model required")
	}
	if t.FinishedAt.IsZero() {
		t.FinishedAt = time.Now()
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = t.FinishedAt
	}

	inCents, outCents, err := computeCosts(ctx, pool, t)
	if err != nil {
		// Non-fatal: log via error, still insert with zero cost.
		// The row has the raw tokens so a later backfill can fix it.
		inCents, outCents = 0, 0
	}

	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO agent_turn_costs
			(agent_id, inbox_id, model, input_tokens, output_tokens,
			 cache_read, cache_write, input_usd_cents, output_usd_cents,
			 started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id
	`, t.AgentID, t.InboxID, t.Model, t.InputTokens, t.OutputTokens,
		t.CacheRead, t.CacheWrite, inCents, outCents,
		t.StartedAt, t.FinishedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("monitor: insert agent_turn_costs: %w", err)
	}
	return id, nil
}

// computeCosts fetches the latest model_rates row at or before
// FinishedAt and returns (input_usd_cents, output_usd_cents).
func computeCosts(ctx context.Context, pool *pgxpool.Pool, t TurnCost) (int, int, error) {
	var inPerMtok, outPerMtok, cacheReadMtok, cacheWriteMtok int
	err := pool.QueryRow(ctx, `
		SELECT input_per_mtok_cents, output_per_mtok_cents,
		       cache_read_per_mtok_cents, cache_write_per_mtok_cents
		FROM model_rates
		WHERE model = $1
		  AND effective_from <= $2
		ORDER BY effective_from DESC
		LIMIT 1
	`, t.Model, t.FinishedAt).Scan(
		&inPerMtok, &outPerMtok, &cacheReadMtok, &cacheWriteMtok,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, errors.New("no rate for model")
		}
		return 0, 0, err
	}

	// Cost in cents = tokens / 1_000_000 * per_mtok_cents.
	// Use integer math with rounding (÷ 1_000_000 after multiply).
	inCents := (t.InputTokens*inPerMtok + t.CacheRead*cacheReadMtok) / 1_000_000
	outCents := (t.OutputTokens*outPerMtok + t.CacheWrite*cacheWriteMtok) / 1_000_000
	return inCents, outCents, nil
}
