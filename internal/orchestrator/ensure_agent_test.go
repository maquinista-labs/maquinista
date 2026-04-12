package orchestrator

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return pool
}

func TestEnsureAgent_HappyPath(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	_, err := pool.Exec(ctx, `
		INSERT INTO tasks (id, title, worktree_path) VALUES ('T-42', 'ship it', $1)
	`, dir)
	if err != nil {
		t.Fatal(err)
	}

	spawnCalls := 0
	spawner := AgentSpawnerFunc(func(ctx context.Context, id, wd, role string) error {
		spawnCalls++
		if wd != dir || role != "implementor" {
			t.Errorf("spawner args: wd=%s role=%s", wd, role)
		}
		return nil
	})

	id, err := EnsureAgent(ctx, EnsureAgentParams{
		Pool: pool, Spawner: spawner,
		Role: "implementor", TaskID: "T-42",
		ObserverUserID: "u1", ObserverThreadID: "100", ObserverChatID: ptrInt64(-1001),
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "impl-T-42" {
		t.Errorf("id=%q, want impl-T-42", id)
	}
	if spawnCalls != 1 {
		t.Errorf("spawner calls=%d", spawnCalls)
	}

	// Agents row present + status working + stop_requested=false.
	var status string
	var stopReq bool
	pool.QueryRow(ctx, `SELECT status, stop_requested FROM agents WHERE id=$1`, id).Scan(&status, &stopReq)
	if status != "working" || stopReq {
		t.Errorf("status=%q stop_requested=%v", status, stopReq)
	}

	// Observer binding present.
	var count int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM topic_agent_bindings
		WHERE agent_id=$1 AND binding_type='observer' AND thread_id='100'
	`, id).Scan(&count)
	if count != 1 {
		t.Errorf("observer bindings=%d, want 1", count)
	}
}

func TestEnsureAgent_MissingWorktreeFailsLoudly(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// worktree_path points at a nonexistent path.
	_, err := pool.Exec(ctx, `INSERT INTO tasks (id, title, worktree_path) VALUES ('T', 'x', '/nonexistent/path/nope')`)
	if err != nil {
		t.Fatal(err)
	}
	spawnCalls := 0
	spawner := AgentSpawnerFunc(func(ctx context.Context, id, wd, role string) error {
		spawnCalls++
		return nil
	})
	_, err = EnsureAgent(ctx, EnsureAgentParams{
		Pool: pool, Spawner: spawner, Role: "implementor", TaskID: "T",
	})
	if err == nil {
		t.Fatal("expected error on missing worktree")
	}
	if spawnCalls != 0 {
		t.Error("spawner must not run when worktree is bad")
	}

	// No agents row was created.
	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE task_id='T'`).Scan(&count)
	if count != 0 {
		t.Errorf("agents=%d, want 0", count)
	}
}

func TestEnsureAgent_Concurrency_UniqueLive(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	_, err := pool.Exec(ctx, `INSERT INTO tasks (id, title, worktree_path) VALUES ('T-50', 'go', $1)`, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Blocking spawner so both inserts can race.
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	spawner := AgentSpawnerFunc(func(ctx context.Context, id, wd, role string) error {
		started <- struct{}{}
		<-release
		return nil
	})

	var wg sync.WaitGroup
	results := make([]struct {
		id  string
		err error
	}, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := EnsureAgent(ctx, EnsureAgentParams{
				Pool: pool, Spawner: spawner,
				Role: "implementor", TaskID: "T-50",
			})
			results[i].id = id
			results[i].err = err
		}(i)
	}

	// Wait for the first spawner to start, then release and unblock.
	<-started
	close(release)
	wg.Wait()

	successCount := 0
	alreadyLiveCount := 0
	for _, r := range results {
		switch {
		case r.err == nil:
			successCount++
		case errors.Is(r.err, ErrAgentAlreadyLive):
			alreadyLiveCount++
			// The sentinel still returns the already-live agent id for reuse.
			if r.id == "" {
				t.Error("ErrAgentAlreadyLive should return the live agent id")
			}
		default:
			t.Errorf("unexpected error: %v", r.err)
		}
	}
	if successCount != 1 || alreadyLiveCount != 1 {
		t.Errorf("success=%d alreadyLive=%d, want 1 and 1", successCount, alreadyLiveCount)
	}
}

func TestEnsureAgent_SpawnerFailureMarksDead(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	dir := t.TempDir()
	pool.Exec(ctx, `INSERT INTO tasks (id, title, worktree_path) VALUES ('T', 'x', $1)`, dir)

	bad := AgentSpawnerFunc(func(ctx context.Context, id, wd, role string) error {
		return errors.New("tmux exploded")
	})
	_, err := EnsureAgent(ctx, EnsureAgentParams{
		Pool: pool, Spawner: bad, Role: "implementor", TaskID: "T",
	})
	if err == nil {
		t.Fatal("expected error from spawner failure")
	}

	// The inserted agents row should be marked dead — retries are unblocked.
	var status string
	pool.QueryRow(ctx, `SELECT status FROM agents WHERE task_id='T'`).Scan(&status)
	if status != "dead" {
		t.Errorf("status=%q, want dead", status)
	}

	// A second EnsureAgent call should succeed because the partial unique
	// index excludes dead rows — the alias bumps to -r2.
	good := AgentSpawnerFunc(func(ctx context.Context, id, wd, role string) error { return nil })
	id, err := EnsureAgent(ctx, EnsureAgentParams{
		Pool: pool, Spawner: good, Role: "implementor", TaskID: "T",
	})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if id != "impl-T-r2" {
		t.Errorf("alias=%q, want impl-T-r2", id)
	}
}

// ensure the tmp dir stat path is reachable on the test host.
func TestEnsureAgent_RequiresExistingWorktreeDir(t *testing.T) {
	if _, err := os.Stat("/tmp"); err != nil {
		t.Skip("no /tmp on this host")
	}
}

func ptrInt64(v int64) *int64 { return &v }
