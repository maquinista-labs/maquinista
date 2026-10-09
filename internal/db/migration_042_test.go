package db

// Migration 042 (MAQ-47): cost_ledger rollup — schema shape, the two
// maintenance triggers (turn accumulation + session span), user/task
// snapshotting, and cascade survival (the reason this is a table, not a
// view over agent_turn_costs, which are ON DELETE CASCADE children of
// the per-round agent rows).

import (
	"context"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func TestMigration042_CostLedger(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	// ── Schema shape ────────────────────────────────────────────────
	wantCols := []struct{ col, dtype string }{
		{"agent_id", "text"},
		{"model", "text"},
		{"user_id", "text"},
		{"task_id", "text"},
		{"turns", "integer"},
		{"input_tokens", "bigint"},
		{"output_tokens", "bigint"},
		{"cache_read_tokens", "bigint"},
		{"cache_write_tokens", "bigint"},
		{"captured_cents", "bigint"},
		{"turn_seconds", "double precision"},
		{"first_turn_at", "timestamp with time zone"},
		{"last_turn_at", "timestamp with time zone"},
		{"session_started_at", "timestamp with time zone"},
		{"turn_end_at", "timestamp with time zone"},
	}
	for _, w := range wantCols {
		var dtype string
		err := pool.QueryRow(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_name='cost_ledger' AND column_name=$1`, w.col,
		).Scan(&dtype)
		if err != nil {
			t.Errorf("cost_ledger.%s missing: %v", w.col, err)
			continue
		}
		if dtype != w.dtype {
			t.Errorf("cost_ledger.%s: type %s, want %s", w.col, dtype, w.dtype)
		}
	}

	// Current-rates view resolves.
	var rateModel string
	if err := pool.QueryRow(ctx,
		`SELECT model FROM v_model_rates_current WHERE model = 'claude-sonnet-4-6'`,
	).Scan(&rateModel); err != nil {
		t.Fatalf("v_model_rates_current: %v", err)
	}

	// ── Fixtures: task + agent + inbox row with a user ──────────────
	start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id, title) VALUES ('task-ledg-1', 'ledger fixture')`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, started_at)
		VALUES ('agent-ledg-1', 's', 'w', 'task-ledg-1', 'working', $1)`, start); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	var inboxID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_inbox (agent_id, from_kind, origin_user_id, origin_thread_id, content)
		VALUES ('agent-ledg-1', 'user', 'user-42', '11', '{"text":"hi"}'::jsonb)
		RETURNING id`).Scan(&inboxID); err != nil {
		t.Fatalf("seed inbox: %v", err)
	}

	finish := start.Add(90 * time.Second)

	// ── Turn 1: accumulates with user/task snapshot + turn seconds ──
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_turn_costs
			(agent_id, inbox_id, model, input_tokens, output_tokens,
			 cache_read, cache_write, input_usd_cents, output_usd_cents,
			 started_at, finished_at)
		VALUES ('agent-ledg-1', $1, 'claude-sonnet-4-6', 1000000, 100000, 500000, 0, 300, 150, $2, $3)`,
		inboxID, start, finish); err != nil {
		t.Fatalf("insert turn 1: %v", err)
	}

	var turns int
	var userID, taskID *string
	var inTok, outTok, captured int64
	var turnSecs float64
	err := pool.QueryRow(ctx, `
		SELECT turns, user_id, task_id, input_tokens, output_tokens, captured_cents, turn_seconds
		FROM cost_ledger WHERE agent_id='agent-ledg-1' AND model='claude-sonnet-4-6'`,
	).Scan(&turns, &userID, &taskID, &inTok, &outTok, &captured, &turnSecs)
	if err != nil {
		t.Fatalf("ledger row after turn 1: %v", err)
	}
	if turns != 1 || inTok != 1000000 || outTok != 100000 {
		t.Errorf("turn 1 counts: turns=%d in=%d out=%d", turns, inTok, outTok)
	}
	if userID == nil || *userID != "user-42" {
		t.Errorf("turn 1 user snapshot: got %v, want user-42", userID)
	}
	if taskID == nil || *taskID != "task-ledg-1" {
		t.Errorf("turn 1 task snapshot: got %v, want task-ledg-1", taskID)
	}
	if captured != 450 {
		t.Errorf("turn 1 captured_cents: got %d, want 450", captured)
	}
	if turnSecs != 90 {
		t.Errorf("turn 1 turn_seconds: got %v, want 90", turnSecs)
	}

	// ── Turn 2 (no inbox): accumulates, snapshot sticks ─────────────
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_turn_costs
			(agent_id, inbox_id, model, input_tokens, output_tokens,
			 cache_read, cache_write, input_usd_cents, output_usd_cents,
			 started_at, finished_at)
		VALUES ('agent-ledg-1', NULL, 'claude-sonnet-4-6', 2000000, 0, 0, 0, 600, 0, $1, $2)`,
		finish, finish.Add(30*time.Second)); err != nil {
		t.Fatalf("insert turn 2: %v", err)
	}
	err = pool.QueryRow(ctx, `
		SELECT turns, user_id, input_tokens, captured_cents, turn_seconds, last_turn_at
		FROM cost_ledger WHERE agent_id='agent-ledg-1' AND model='claude-sonnet-4-6'`,
	).Scan(&turns, &userID, &inTok, &captured, &turnSecs, new(time.Time))
	if err != nil {
		t.Fatalf("ledger row after turn 2: %v", err)
	}
	if turns != 2 || inTok != 3000000 || captured != 1050 || turnSecs != 120 {
		t.Errorf("turn 2 accumulation: turns=%d in=%d captured=%d secs=%v",
			turns, inTok, captured, turnSecs)
	}
	if userID == nil || *userID != "user-42" {
		t.Errorf("turn 2 kept user snapshot: got %v, want user-42", userID)
	}

	// ── Turn-end event: session span lands on the ledger ────────────
	turnEnd := finish.Add(10 * time.Minute)
	if _, err := pool.Exec(ctx, `
		UPDATE agents SET last_turn_end_at = $1 WHERE id = 'agent-ledg-1'`,
		turnEnd); err != nil {
		t.Fatalf("turn-end update: %v", err)
	}
	var span *float64
	if err := pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM (turn_end_at - session_started_at))
		FROM cost_ledger WHERE agent_id='agent-ledg-1' AND model='claude-sonnet-4-6'`,
	).Scan(&span); err != nil {
		t.Fatalf("span query: %v", err)
	}
	if span == nil || *span != 690 { // agents.started_at 10:00:00 → turn_end 10:11:30
		var got float64
		if span != nil {
			got = *span
		}
		t.Errorf("session span: got %v, want 690", got)
	}

	// ── Cascade survival: killing the agent must not lose the ledger ─
	if _, err := pool.Exec(ctx, `DELETE FROM agents WHERE id = 'agent-ledg-1'`); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	var gotUser, gotTask *string
	var gotTurns int
	if err := pool.QueryRow(ctx, `
		SELECT turns, user_id, task_id FROM cost_ledger
		WHERE agent_id='agent-ledg-1' AND model='claude-sonnet-4-6'`,
	).Scan(&gotTurns, &gotUser, &gotTask); err != nil {
		t.Fatalf("ledger row must survive agent deletion (cascade): %v", err)
	}
	if gotTurns != 2 || gotUser == nil || *gotUser != "user-42" || gotTask == nil || *gotTask != "task-ledg-1" {
		t.Errorf("post-delete snapshots: turns=%d user=%v task=%v", gotTurns, gotUser, gotTask)
	}

	// Clean the fixture so rollup tests start fresh is unnecessary —
	// each test gets its own container.
}
