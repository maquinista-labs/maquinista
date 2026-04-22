package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// seedAgent inserts a minimal agents row satisfying the FK on agent_turn_costs.
func seedAgent(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, status, runner_type, role)
		VALUES ($1,'s','0:1','working','claude','executor')
		ON CONFLICT DO NOTHING
	`, id); err != nil {
		t.Fatalf("seedAgent %q: %v", id, err)
	}
}

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

	seedAgent(t, pool, "cost-agent")

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

	seedAgent(t, pool, "cost-agent-u")

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

// TestCaptureTurn_NotifyFires verifies migration 030: a pg_notify on
// channel "agent_turn_cost_new" fires within 1 s of CaptureTurn.
// The payload must equal the agent_id that was inserted.
func TestCaptureTurn_NotifyFires(t *testing.T) {
	_, dsn := dbtest.PgContainer(t)
	// Apply migrations to the fresh container.
	pool2, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool2.Close)
	if _, err := db.RunMigrations(pool2); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	seedAgent(t, pool2, "notify-agent")

	// Open a dedicated LISTEN connection (can't reuse a pool connection
	// for LISTEN — the conn must stay checked out for the duration).
	conn, err := pool2.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN agent_turn_cost_new"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	// Fire the insert from the pool (a different connection).
	go func() {
		_, _ = CaptureTurn(context.Background(), pool2, TurnCost{
			AgentID:      "notify-agent",
			Model:        "test-notify-model",
			InputTokens:  100,
			OutputTokens: 50,
			FinishedAt:   time.Now(),
		})
	}()

	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	notif, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v (trigger may be missing — run migration 030)", err)
	}
	if notif.Channel != "agent_turn_cost_new" {
		t.Errorf("channel = %q; want agent_turn_cost_new", notif.Channel)
	}
	if notif.Payload != "notify-agent" {
		t.Errorf("payload = %q; want notify-agent", notif.Payload)
	}
}

// TestScheduledJobsNotify verifies migration 031: INSERT, UPDATE, and DELETE
// on scheduled_jobs each emit a pg_notify on "scheduled_jobs_change".
func TestScheduledJobsNotify(t *testing.T) {
	_, dsn := dbtest.PgContainer(t)
	pool2, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool2.Close)
	if _, err := db.RunMigrations(pool2); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	seedAgent(t, pool2, "jobs-notify-agent")

	conn, err := pool2.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN scheduled_jobs_change"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	expectNotif := func(op string) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		notif, err := conn.Conn().WaitForNotification(waitCtx)
		if err != nil {
			t.Fatalf("%s: WaitForNotification: %v", op, err)
		}
		if notif.Channel != "scheduled_jobs_change" {
			t.Errorf("%s: channel = %q; want scheduled_jobs_change", op, notif.Channel)
		}
		if notif.Payload == "" {
			t.Errorf("%s: empty payload; expected row id", op)
		}
	}

	// INSERT
	var jobID string
	if err := pool2.QueryRow(ctx, `
		INSERT INTO scheduled_jobs
			(name, cron_expr, agent_id, prompt, enabled, next_run_at)
		VALUES ('test-job','0 * * * *','jobs-notify-agent','{}',true,now()+interval'1h')
		RETURNING id
	`).Scan(&jobID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	expectNotif("INSERT")

	// UPDATE
	if _, err := pool2.Exec(ctx,
		`UPDATE scheduled_jobs SET enabled=false WHERE id=$1`, jobID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	expectNotif("UPDATE")

	// DELETE
	if _, err := pool2.Exec(ctx,
		`DELETE FROM scheduled_jobs WHERE id=$1`, jobID,
	); err != nil {
		t.Fatalf("delete: %v", err)
	}
	expectNotif("DELETE")
}

// TestWebhookHandlersNotify verifies migration 031 for webhook_handlers.
func TestWebhookHandlersNotify(t *testing.T) {
	_, dsn := dbtest.PgContainer(t)
	pool2, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool2.Close)
	if _, err := db.RunMigrations(pool2); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	seedAgent(t, pool2, "wh-notify-agent")

	conn, err := pool2.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN webhook_handlers_change"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	expectNotif := func(op string) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		notif, err := conn.Conn().WaitForNotification(waitCtx)
		if err != nil {
			t.Fatalf("%s: WaitForNotification: %v", op, err)
		}
		if notif.Channel != "webhook_handlers_change" {
			t.Errorf("%s: channel = %q; want webhook_handlers_change", op, notif.Channel)
		}
		if notif.Payload == "" {
			t.Errorf("%s: empty payload; expected row id", op)
		}
	}

	var whID string
	if err := pool2.QueryRow(ctx, `
		INSERT INTO webhook_handlers
			(name, path, secret, agent_id, prompt_template, enabled)
		VALUES ('test-wh','/hook/test','s','wh-notify-agent','{{ payload }}',true)
		RETURNING id
	`).Scan(&whID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	expectNotif("INSERT")

	if _, err := pool2.Exec(ctx,
		`UPDATE webhook_handlers SET enabled=false WHERE id=$1`, whID,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	expectNotif("UPDATE")

	if _, err := pool2.Exec(ctx,
		`DELETE FROM webhook_handlers WHERE id=$1`, whID,
	); err != nil {
		t.Fatalf("delete: %v", err)
	}
	expectNotif("DELETE")
}
