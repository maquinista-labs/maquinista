package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return pool
}

func TestCaptureTurn_InsertsRowWithComputedCents(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// Seed a model_rates row (migration also seeds some, but use
	// a stable ballpark for assertions).
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_rates
			(model, input_per_mtok_cents, output_per_mtok_cents,
			 cache_read_per_mtok_cents, cache_write_per_mtok_cents,
			 effective_from)
		VALUES ('test-model', 1000, 5000, 100, 1250, '2025-01-01')
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("seed model_rates: %v", err)
	}

	// Insert a minimum agents row so the FK is satisfied.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, status,
		                    runner_type, role)
		VALUES ('cost-agent','s','0:1','working','claude','executor')
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	started := time.Now().Add(-time.Second)
	finished := time.Now()
	id, err := CaptureTurn(ctx, pool, TurnCost{
		AgentID:      "cost-agent",
		Model:        "test-model",
		InputTokens:  1_500_000, // 1.5 M tokens
		OutputTokens: 500_000,   // 500 k
		StartedAt:    started,
		FinishedAt:   finished,
	})
	if err != nil {
		t.Fatalf("CaptureTurn: %v", err)
	}
	if id == 0 {
		t.Fatal("CaptureTurn returned id=0")
	}

	var inC, outC, inTok, outTok int
	if err := pool.QueryRow(ctx, `
		SELECT input_usd_cents, output_usd_cents,
		       input_tokens, output_tokens
		FROM agent_turn_costs WHERE id = $1
	`, id).Scan(&inC, &outC, &inTok, &outTok); err != nil {
		t.Fatalf("read: %v", err)
	}
	// 1.5 M @ 1000 ¢/Mtok = 1500 ¢
	if inC != 1500 {
		t.Errorf("input_usd_cents = %d; want 1500", inC)
	}
	// 500 k @ 5000 ¢/Mtok = 2500 ¢
	if outC != 2500 {
		t.Errorf("output_usd_cents = %d; want 2500", outC)
	}
	if inTok != 1_500_000 || outTok != 500_000 {
		t.Errorf("token counts wrong: %d / %d", inTok, outTok)
	}
}

func TestCaptureTurn_UnknownModelStillInserts(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, status,
		                    runner_type, role)
		VALUES ('cost-agent-u','s','0:1','working','claude','executor')
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	now := time.Now()
	id, err := CaptureTurn(ctx, pool, TurnCost{
		AgentID:    "cost-agent-u",
		Model:      "unknown-model-xyz",
		InputTokens:  100,
		OutputTokens: 200,
		StartedAt:    now,
		FinishedAt:   now,
	})
	if err != nil {
		t.Fatalf("CaptureTurn: %v", err)
	}
	var inC, outC int
	if err := pool.QueryRow(ctx, `
		SELECT input_usd_cents, output_usd_cents
		FROM agent_turn_costs WHERE id = $1
	`, id).Scan(&inC, &outC); err != nil {
		t.Fatalf("read: %v", err)
	}
	if inC != 0 || outC != 0 {
		t.Errorf("unknown model should insert with 0 cents; got %d/%d", inC, outC)
	}
}

func TestCaptureTurn_RequiresAgentAndModel(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	if _, err := CaptureTurn(ctx, pool, TurnCost{Model: "m"}); err == nil {
		t.Fatal("missing AgentID did not error")
	}
	if _, err := CaptureTurn(ctx, pool, TurnCost{AgentID: "a"}); err == nil {
		t.Fatal("missing Model did not error")
	}
}
