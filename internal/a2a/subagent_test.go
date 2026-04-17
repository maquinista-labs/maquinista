package a2a

import (
	"context"
	"errors"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/soul"
)

func TestSpawnSubagent_RequiresAllowDelegation(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('parent','s','p')`); err != nil {
		t.Fatal(err)
	}
	// Soul with allow_delegation=false (the default).
	if err := soul.CreateFromTemplate(ctx, pool, "parent", "", soul.Overrides{}); err != nil {
		t.Fatal(err)
	}

	_, err := SpawnSubagent(ctx, pool, "parent", "do a thing", nil)
	if !errors.Is(err, ErrDelegationDenied) {
		t.Errorf("want ErrDelegationDenied, got %v", err)
	}
}

func TestSpawnSubagent_CreatesAgentAndConversation(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('parent','s','p')`); err != nil {
		t.Fatal(err)
	}
	// Flip allow_delegation=TRUE on the default template-derived soul.
	if err := soul.CreateFromTemplate(ctx, pool, "parent", "", soul.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_souls SET allow_delegation=TRUE WHERE agent_id='parent'`); err != nil {
		t.Fatal(err)
	}

	var spawnedChild string
	spawn := func(ctx context.Context, parentID, childID, goal string) error {
		spawnedChild = childID
		return nil
	}

	handle, err := SpawnSubagent(ctx, pool, "parent", "fetch the readme", spawn)
	if err != nil {
		t.Fatalf("SpawnSubagent: %v", err)
	}
	if handle.ChildID == "" {
		t.Fatal("empty ChildID")
	}
	if spawnedChild != handle.ChildID {
		t.Errorf("SpawnFunc called with %q, ChildID=%q", spawnedChild, handle.ChildID)
	}

	// agents row created?
	var role string
	pool.QueryRow(ctx, `SELECT role FROM agents WHERE id=$1`, handle.ChildID).Scan(&role)
	if role != "executor" {
		t.Errorf("child role=%q, want executor", role)
	}

	// Conversation inserted with correct participants?
	var kind string
	pool.QueryRow(ctx, `SELECT kind FROM conversations WHERE id=$1`, handle.ConversationID).Scan(&kind)
	if kind != "a2a" {
		t.Errorf("kind=%q, want a2a", kind)
	}

	// Inbox row for the child carrying the goal?
	var inboxCount int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_inbox
		WHERE agent_id=$1 AND from_kind='agent' AND from_id='parent'
	`, handle.ChildID).Scan(&inboxCount)
	if inboxCount != 1 {
		t.Errorf("child inbox rows=%d, want 1", inboxCount)
	}

	// Soul cloned onto the child with allow_delegation reset to false?
	var childAllow bool
	pool.QueryRow(ctx, `SELECT allow_delegation FROM agent_souls WHERE agent_id=$1`, handle.ChildID).Scan(&childAllow)
	if childAllow {
		t.Error("child soul should have allow_delegation=FALSE so grandchildren aren't free to spawn")
	}
}

func TestSpawnSubagent_ReturnsTimeoutErrOnWait(t *testing.T) {
	// Smoke-test WaitForResult's timeout path: no outbox, small timeout.
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('parent','s','p')`); err != nil {
		t.Fatal(err)
	}
	if err := soul.CreateFromTemplate(ctx, pool, "parent", "", soul.Overrides{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_souls SET allow_delegation=TRUE WHERE agent_id='parent'`); err != nil {
		t.Fatal(err)
	}

	handle, err := SpawnSubagent(ctx, pool, "parent", "never answered", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WaitForResult(ctx, pool, handle.ConversationID, handle.ChildID, 700_000_000 /* ns = 0.7s */); !errors.Is(err, ErrTimeout) {
		t.Errorf("want ErrTimeout, got %v", err)
	}
}
