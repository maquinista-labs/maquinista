package db

import (
	"context"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func TestMigration034_LinearIssueMap(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	applied, err := RunMigrations(pool)
	if err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	has := func(name string) bool {
		for _, n := range applied {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("034_linear_bridge.sql") {
		t.Fatalf("034_linear_bridge.sql was not applied; got %v", applied)
	}
	ctx := context.Background()

	want := []struct{ col, dtype, nullable string }{
		{"linear_issue_id", "text", "NO"},
		{"identifier", "text", "NO"},
		{"team_id", "text", "NO"},
		{"task_id", "text", "NO"},
		{"last_synced_state", "text", "YES"},
		{"pending_state", "text", "YES"},
		{"attempts", "integer", "NO"},
		{"next_attempt_at", "timestamp with time zone", "NO"},
		{"synced_at", "timestamp with time zone", "YES"},
		{"created_at", "timestamp with time zone", "NO"},
		{"updated_at", "timestamp with time zone", "NO"},
	}
	for _, w := range want {
		var dtype, nullable string
		var dflt *string
		err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable, column_default
			FROM information_schema.columns
			WHERE table_name='linear_issue_map' AND column_name=$1`, w.col,
		).Scan(&dtype, &nullable, &dflt)
		if err != nil {
			t.Errorf("linear_issue_map.%s missing: %v", w.col, err)
			continue
		}
		if dtype != w.dtype || nullable != w.nullable {
			t.Errorf("linear_issue_map.%s: got %s nullable=%s, want %s nullable=%s",
				w.col, dtype, nullable, w.dtype, w.nullable)
		}
		switch w.col {
		case "attempts":
			if dflt == nil || *dflt != "0" {
				t.Errorf("linear_issue_map.attempts default: got %v, want 0", dflt)
			}
		case "next_attempt_at", "created_at", "updated_at":
			if dflt == nil || !strings.Contains(*dflt, "now()") {
				t.Errorf("linear_issue_map.%s default: got %v, want now()", w.col, dflt)
			}
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid
		WHERE r.relname = 'linear_issue_map' AND c.contype = 'p'`,
	).Scan(&n); err != nil || n != 1 {
		t.Errorf("linear_issue_map primary key: n=%d err=%v", n, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid
		WHERE r.relname = 'linear_issue_map' AND c.contype = 'u'`,
	).Scan(&n); err != nil || n != 1 {
		t.Errorf("linear_issue_map UNIQUE(task_id): n=%d err=%v", n, err)
	}
}

func TestMigration034_ReviewRounds(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	var dtype string
	var dflt *string
	err := pool.QueryRow(ctx, `
		SELECT data_type, column_default FROM information_schema.columns
		WHERE table_name='tasks' AND column_name='review_rounds'`,
	).Scan(&dtype, &dflt)
	if err != nil {
		t.Fatalf("tasks.review_rounds missing: %v", err)
	}
	if dtype != "integer" {
		t.Errorf("tasks.review_rounds type: got %s, want integer", dtype)
	}
	if dflt == nil || *dflt != "0" {
		t.Errorf("tasks.review_rounds default: got %v, want 0", dflt)
	}

	mustExec(t, pool, `INSERT INTO tasks (id, title) VALUES ('rr1', 'rounds default')`)
	var rr int
	if err := pool.QueryRow(ctx, `SELECT review_rounds FROM tasks WHERE id='rr1'`).Scan(&rr); err != nil {
		t.Fatalf("select review_rounds: %v", err)
	}
	if rr != 0 {
		t.Errorf("review_rounds on fresh task: got %d, want 0", rr)
	}
}

func TestMigration034_Cascade(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO tasks (id, title) VALUES ('t1', 'cascade me')`)
	mustExec(t, pool, `
		INSERT INTO linear_issue_map (linear_issue_id, identifier, team_id, task_id)
		VALUES ('li-1', 'MAQ-1', 'team-maq', 't1')`)

	// UNIQUE(task_id): a second map row for the same task must be refused.
	if _, err := pool.Exec(ctx, `
		INSERT INTO linear_issue_map (linear_issue_id, identifier, team_id, task_id)
		VALUES ('li-2', 'MAQ-2', 'team-maq', 't1')`); err == nil {
		t.Errorf("second linear_issue_map row for the same task_id should violate UNIQUE")
	}

	mustExec(t, pool, `DELETE FROM tasks WHERE id='t1'`)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM linear_issue_map WHERE task_id='t1'`).Scan(&n); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("cascade did not remove linear_issue_map rows: n=%d", n)
	}
}
