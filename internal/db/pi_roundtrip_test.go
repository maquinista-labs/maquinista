package db

import (
	"context"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// TestRegisterAgent_RunnerTypeRoundTrip: runner_type "pi" is stored and
// read back unchanged — the registry key and the DB value must agree
// verbatim (C21).
func TestRegisterAgent_RunnerTypeRoundTrip(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	if err := RegisterAgent(pool, "agent-rt-1", "maq", "w1", nil, nil, "pi", nil, ""); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT runner_type FROM agents WHERE id='agent-rt-1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "pi" {
		t.Errorf("runner_type = %q, want %q (unchanged)", got, "pi")
	}
}

// TestRegisterAgent_SessionMapKeyProjection: after the spawn-path write,
// state.LoadSessionMap yields the <tmuxSession>:<windowID> key with
// session_id still empty for a hookless runner (C23 — the OC-03 fallback
// contract pi rides on).
func TestRegisterAgent_SessionMapKeyProjection(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	if err := RegisterAgent(pool, "agent-proj-1", "maq", "w7", nil, nil, "pi", nil, "executor"); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	sm, err := state.LoadSessionMap(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSessionMap: %v", err)
	}
	entry, ok := sm["maq:w7"]
	if !ok {
		t.Fatalf("key maq:w7 missing from session map (%d entries)", len(sm))
	}
	if entry.SessionID != "" {
		t.Errorf("session_id = %q, want empty (hookless: no backfill yet)", entry.SessionID)
	}
}
