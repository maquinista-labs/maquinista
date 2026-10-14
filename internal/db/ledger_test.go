package db

// LedgerRollup (MAQ-47): grain switching (session/task/user), current-
// rate re-pricing vs captured cents, filters, sentinels, and ordering.

import (
	"context"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func TestLedgerRollup(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql[:40], err)
		}
	}

	// Rates: give the test model an exact current rate; second model
	// gets none → its group's CurrentCents must be nil.
	seed(`INSERT INTO model_rates
		(model, input_per_mtok_cents, output_per_mtok_cents, cache_read_per_mtok_cents, cache_write_per_mtok_cents, effective_from)
		VALUES ('ledg-model', 100, 1000, 0, 0, '2025-06-01')`)

	seed(`INSERT INTO tasks (id, title) VALUES ('ledg-task-1', 'one'), ('ledg-task-2', 'two')`)
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	seed(`INSERT INTO agents (id, tmux_session, tmux_window, task_id, started_at) VALUES
		('ledg-agent-a', 's', 'wa', 'ledg-task-1', $1),
		('ledg-agent-b', 's', 'wb', 'ledg-task-2', $1),
		('ledg-agent-interactive', 's', 'wi', NULL, $1)`, t0)

	// Inbox row attributes the interactive session to a user.
	var inboxID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_inbox (agent_id, from_kind, origin_user_id, origin_thread_id, content)
		VALUES ('ledg-agent-interactive', 'user', 'ledg-user-7', '3', '{"text":"hi"}'::jsonb)
		RETURNING id`).Scan(&inboxID); err != nil {
		t.Fatalf("seed inbox: %v", err)
	}

	turn := func(agent, model string, in, out int, inC, outC int, secs int, at time.Time, inbox *string) {
		t.Helper()
		seed(`INSERT INTO agent_turn_costs
			(agent_id, inbox_id, model, input_tokens, output_tokens,
			 cache_read, cache_write, input_usd_cents, output_usd_cents,
			 started_at, finished_at)
			VALUES ($1,$2,$3,$4,$5,0,0,$6,$7,$8,$9)`,
			agent, inbox, model, in, out, inC, outC, at, at.Add(time.Duration(secs)*time.Second))
	}

	turn("ledg-agent-interactive", "ledg-model", 1_000_000, 0, 100, 0, 45, t0, &inboxID)

	// Task 1, priced model: 1M in + 100k out per turn.
	// current per turn: 1_000_000*100/1e6 + 100_000*1000/1e6 = 100+100 = 200.
	turn("ledg-agent-a", "ledg-model", 1_000_000, 100_000, 100, 100, 60, t0, nil)
	turn("ledg-agent-a", "ledg-model", 1_000_000, 100_000, 100, 100, 90, t0.Add(time.Minute), nil)
	// Task 2, unpriced model → group current must be nil.
	turn("ledg-agent-b", "ledg-no-rate-model", 500_000, 0, 50, 0, 30, t0, nil)

	// Turn-end event on agent-a: session span 9:00:00 → 9:05:00.
	seed(`UPDATE agents SET last_turn_end_at = $1 WHERE id = 'ledg-agent-a'`,
		t0.Add(5*time.Minute))

	// ── Session grain ───────────────────────────────────────────────
	rows, err := LedgerRollup(ctx, pool, LedgerBySession, nil, nil, nil, 25)
	if err != nil {
		t.Fatalf("LedgerRollup(session): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("session rows: got %d, want 3: %+v", len(rows), rows)
	}
	byAgent := map[string]LedgerRow{}
	for _, r := range rows {
		byAgent[*r.AgentID] = r
	}
	a := byAgent["ledg-agent-a"]
	if a.Turns != 2 || a.InputTokens != 2_000_000 || a.CapturedCents != 400 || a.TurnSeconds != 150 {
		t.Errorf("agent-a: %+v", a)
	}
	if a.CurrentCents == nil || *a.CurrentCents != 400 {
		t.Errorf("agent-a current: got %v, want 400", a.CurrentCents)
	}
	if a.WallSeconds == nil || *a.WallSeconds != 300 {
		var got float64
		if a.WallSeconds != nil {
			got = *a.WallSeconds
		}
		t.Errorf("agent-a span: got %v, want 300", got)
	}
	b := byAgent["ledg-agent-b"]
	if b.CurrentCents != nil {
		t.Errorf("agent-b current must be nil (no rate for model): %v", *b.CurrentCents)
	}
	if b.CapturedCents != 50 {
		t.Errorf("agent-b captured: got %d, want 50", b.CapturedCents)
	}
	i := byAgent["ledg-agent-interactive"]
	if i.UserID == nil || *i.UserID != "ledg-user-7" {
		t.Errorf("interactive user: got %v, want ledg-user-7", i.UserID)
	}
	if i.TaskID != nil {
		t.Errorf("interactive task: got %v, want NULL", *i.TaskID)
	}

	// ── Task grain ──────────────────────────────────────────────────
	rows, err = LedgerRollup(ctx, pool, LedgerByTask, nil, nil, nil, 25)
	if err != nil {
		t.Fatalf("LedgerRollup(task): %v", err)
	}
	byKey := map[string]LedgerRow{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	t1 := byKey["ledg-task-1"]
	if t1.Turns != 2 || t1.CapturedCents != 400 {
		t.Errorf("task-1: %+v", t1)
	}
	if t1.UserID != nil {
		t.Errorf("task-1 user (agent-a has no inbox attribution): got %v, want NULL", *t1.UserID)
	}
	ti := byKey[LedgerInteractive]
	if ti.Turns != 1 || ti.TaskID != nil {
		t.Errorf("interactive task grain: %+v", ti)
	}
	// Ordering: task-1 (400) before interactive (100) before task-2 (50,
	// unpriced → captured fallback).
	if !(rows[0].Key == "ledg-task-1" && rows[1].Key == LedgerInteractive && rows[2].Key == "ledg-task-2") {
		t.Errorf("task ordering by cost desc: %s, %s, %s",
			rows[0].Key, rows[1].Key, rows[2].Key)
	}

	// ── User grain ──────────────────────────────────────────────────
	rows, err = LedgerRollup(ctx, pool, LedgerByUser, nil, nil, nil, 25)
	if err != nil {
		t.Fatalf("LedgerRollup(user): %v", err)
	}
	byKey = map[string]LedgerRow{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	u := byKey["ledg-user-7"]
	if u.Turns != 1 || u.CapturedCents != 100 {
		t.Errorf("user-7: %+v", u)
	}
	pipeline := byKey[LedgerNoUser]
	if pipeline.Turns != 3 { // agents a + b have no inbox attribution
		t.Errorf("(unattributed): %+v", pipeline)
	}

	// ── Filters ─────────────────────────────────────────────────────
	taskFilter := "ledg-task-1"
	rows, err = LedgerRollup(ctx, pool, LedgerBySession, nil, &taskFilter, nil, 25)
	if err != nil || len(rows) != 1 || *rows[0].AgentID != "ledg-agent-a" {
		t.Errorf("task filter: rows=%v err=%v", rows, err)
	}
	userFilter := "ledg-user-7"
	rows, err = LedgerRollup(ctx, pool, LedgerBySession, &userFilter, nil, nil, 25)
	if err != nil || len(rows) != 1 || *rows[0].AgentID != "ledg-agent-interactive" {
		t.Errorf("user filter: rows=%v err=%v", rows, err)
	}

	// ── Since filter (activity window on last_turn_at = finished_at) ─
	after := t0.Add(90 * time.Second) // only agent-a reaches t0+150s
	rows, err = LedgerRollup(ctx, pool, LedgerBySession, nil, nil, &after, 25)
	if err != nil {
		t.Fatalf("since filter: %v", err)
	}
	if len(rows) != 1 || *rows[0].AgentID != "ledg-agent-a" {
		t.Errorf("since filter: got %d rows, want agent-a only", len(rows))
	}

	// ── Limit + bad grain ───────────────────────────────────────────
	rows, err = LedgerRollup(ctx, pool, LedgerByTask, nil, nil, nil, 1)
	if err != nil || len(rows) != 1 {
		t.Errorf("limit: rows=%d err=%v", len(rows), err)
	}
	if _, err = LedgerRollup(ctx, pool, "bogus", nil, nil, nil, 25); err == nil {
		t.Error("bogus grain must error")
	}
}
