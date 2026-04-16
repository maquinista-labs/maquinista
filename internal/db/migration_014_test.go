package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// Migrations 013 + 014 land together per plans/per-topic-agent-pivot.md:
// 013 drops agent_settings.is_default, 014 adds agents.handle with a
// partial unique index on LOWER(handle). Both are exercised here.

func TestMigration013_DropsIsDefault(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	var col string
	err := pool.QueryRow(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_name='agent_settings' AND column_name='is_default'
	`).Scan(&col)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("is_default still present after migration 013: err=%v col=%q", err, col)
	}

	// The uniqueness index is also gone.
	var idxName string
	err = pool.QueryRow(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE tablename='agent_settings' AND indexname='uq_agent_settings_is_default'
	`).Scan(&idxName)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("uq_agent_settings_is_default index still present: err=%v name=%q", err, idxName)
	}
}

func TestMigration014_HandleColumn(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	ctx := context.Background()
	var col string
	if err := pool.QueryRow(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_name='agents' AND column_name='handle'
	`).Scan(&col); err != nil {
		t.Fatalf("agents.handle missing: %v", err)
	}

	// Seed two agents and verify the partial unique index on LOWER(handle)
	// blocks duplicates (case-insensitive) while allowing multiple NULLs.
	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w1'),('b','s','w2'),('c','s','w3')`)

	if _, err := pool.Exec(ctx, `UPDATE agents SET handle='pilot' WHERE id='a'`); err != nil {
		t.Fatalf("set handle a: %v", err)
	}
	// Multiple nulls are fine (partial index excludes them).
	if _, err := pool.Exec(ctx, `UPDATE agents SET handle=NULL WHERE id='b'`); err != nil {
		t.Fatalf("clear handle b: %v", err)
	}

	// Case-insensitive conflict on 'Pilot' vs 'pilot'.
	_, err := pool.Exec(ctx, `UPDATE agents SET handle='Pilot' WHERE id='c'`)
	if err == nil {
		t.Fatal("expected unique-violation on case-insensitive duplicate handle")
	}
	if !strings.Contains(err.Error(), "uq_agents_handle_lower") {
		t.Errorf("error not from the handle index: %v", err)
	}
}
