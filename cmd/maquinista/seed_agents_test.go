package main

import (
	"context"
	"os"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// TestSeedDefaultAgents covers the three behaviours of the G.4
// first-boot seed: happy path (empty DB), idempotency (pre-existing
// row), and the MAQUINISTA_SKIP_SEED_AGENTS opt-out.
//
// Each sub-test uses a dedicated Postgres container (dbtest.PgContainer)
// so state from one does not leak into another — cheap enough for a
// handful of cases.

func TestSeedDefaultAgents_EmptyDB(t *testing.T) {
	t.Setenv("MAQUINISTA_SKIP_SEED_AGENTS", "")

	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	cfg := &config.Config{
		TmuxSessionName: "maquinista-test",
		DefaultRunner:   "claude",
	}
	ctx := context.Background()
	if err := seedDefaultAgents(ctx, cfg, pool, t.TempDir()); err != nil {
		t.Fatalf("seedDefaultAgents: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agents WHERE id IN ('seed-coordinator','seed-planner','seed-coder')
	`).Scan(&count); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if count != 3 {
		t.Fatalf("agents seeded = %d, want 3", count)
	}

	// Handles point at the friendly names.
	var handles []string
	rows, err := pool.Query(ctx, `
		SELECT handle FROM agents
		WHERE id IN ('seed-coordinator','seed-planner','seed-coder')
		ORDER BY handle
	`)
	if err != nil {
		t.Fatalf("query handles: %v", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		handles = append(handles, h)
	}
	want := []string{"coder", "coordinator", "planner"}
	if len(handles) != 3 || handles[0] != want[0] || handles[1] != want[1] || handles[2] != want[2] {
		t.Fatalf("handles = %v, want %v", handles, want)
	}

	// Souls are cloned from the matching templates, not default.
	var tplIDs []string
	r2, err := pool.Query(ctx, `
		SELECT template_id FROM agent_souls
		WHERE agent_id IN ('seed-coordinator','seed-planner','seed-coder')
		ORDER BY template_id
	`)
	if err != nil {
		t.Fatalf("query souls: %v", err)
	}
	for r2.Next() {
		var id string
		if err := r2.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tplIDs = append(tplIDs, id)
	}
	wantTpl := []string{"coder", "coordinator", "planner"}
	if len(tplIDs) != 3 || tplIDs[0] != wantTpl[0] || tplIDs[1] != wantTpl[1] || tplIDs[2] != wantTpl[2] {
		t.Fatalf("soul template_ids = %v, want %v", tplIDs, wantTpl)
	}

	// Seeded rows are status='stopped' so reconcile brings their panes up.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='seed-coder'`).Scan(&status); err != nil {
		t.Fatalf("seed-coder status: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("seed-coder status=%q, want 'stopped'", status)
	}
}

func TestSeedDefaultAgents_Idempotent(t *testing.T) {
	t.Setenv("MAQUINISTA_SKIP_SEED_AGENTS", "")

	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	// Pre-seed one row with a distinguishing handle and role so we
	// can prove the helper doesn't clobber it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agents
			(id, handle, tmux_session, tmux_window, role, status,
			 runner_type, cwd, window_name, started_at, last_seen,
			 stop_requested)
		VALUES ('seed-coordinator', 'custom-handle', 'ses', 'w1',
		        'user', 'idle', 'claude', '/tmp/cust', 'seed-coordinator',
		        NOW(), NOW(), FALSE)
	`); err != nil {
		t.Fatalf("pre-insert: %v", err)
	}

	cfg := &config.Config{
		TmuxSessionName: "maquinista-test",
		DefaultRunner:   "claude",
	}
	if err := seedDefaultAgents(ctx, cfg, pool, t.TempDir()); err != nil {
		t.Fatalf("seedDefaultAgents: %v", err)
	}

	// Pre-existing row is untouched.
	var h, cwd, status string
	if err := pool.QueryRow(ctx,
		`SELECT handle, cwd, status FROM agents WHERE id='seed-coordinator'`,
	).Scan(&h, &cwd, &status); err != nil {
		t.Fatalf("query: %v", err)
	}
	if h != "custom-handle" {
		t.Errorf("handle clobbered: got %q, want 'custom-handle'", h)
	}
	if cwd != "/tmp/cust" {
		t.Errorf("cwd clobbered: got %q, want '/tmp/cust'", cwd)
	}
	if status != "idle" {
		t.Errorf("status clobbered: got %q, want 'idle'", status)
	}

	// The other two are still seeded fresh.
	var other int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agents
		WHERE id IN ('seed-planner','seed-coder')
	`).Scan(&other); err != nil {
		t.Fatalf("count others: %v", err)
	}
	if other != 2 {
		t.Fatalf("seed-planner/coder count = %d, want 2", other)
	}
}

func TestSeedDefaultAgents_SkipEnv(t *testing.T) {
	t.Setenv("MAQUINISTA_SKIP_SEED_AGENTS", "1")

	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	cfg := &config.Config{
		TmuxSessionName: "maquinista-test",
		DefaultRunner:   "claude",
	}
	if err := seedDefaultAgents(context.Background(), cfg, pool, t.TempDir()); err != nil {
		t.Fatalf("seedDefaultAgents: %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("SKIP set, expected 0 agents, got %d", n)
	}
}

func TestSeedDefaultAgents_TemplatesFromMigration028(t *testing.T) {
	// Guard that migration 028 shipped the three templates this helper
	// depends on.
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM soul_templates
		WHERE id IN ('coordinator','planner','coder')
	`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 3 {
		t.Fatalf("migration 028 templates = %d, want 3", n)
	}
}

// avoid 'unused import' lints if this file is the only consumer
// of `os` in the package.
var _ = os.Getenv
