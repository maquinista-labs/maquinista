package taskscheduler

import (
	"context"
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

func TestDispatchOne_ReadyTaskFlow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path) VALUES ('T', 'x', 'ready', $1)`, dir)

	ensured := ""
	cfg := Config{
		EnsureAgent: func(ctx context.Context, role, taskID string) (string, error) {
			ensured = taskID
			// Insert a live agents row to simulate the real EnsureAgent's side effect.
			_, err := pool.Exec(ctx, `
				INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
				VALUES ($1, 'maquinista', $1, $2, 'working', $3)
			`, "impl-"+taskID, taskID, role)
			if err != nil {
				return "", err
			}
			return "impl-" + taskID, nil
		},
	}

	ok, err := DispatchOne(ctx, pool, cfg)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ensured != "T" {
		t.Errorf("ensured=%q", ensured)
	}

	// Task flipped to 'claimed', claimed_by set.
	var status, claimedBy string
	pool.QueryRow(ctx, `SELECT status, COALESCE(claimed_by,'') FROM tasks WHERE id='T'`).Scan(&status, &claimedBy)
	if status != "claimed" || claimedBy != "@impl-T" {
		t.Errorf("status=%q claimed_by=%q", status, claimedBy)
	}

	// Inbox row enqueued.
	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE origin_channel='task' AND external_msg_id='task:T'`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1", count)
	}
}

func TestDispatchOne_DAGCascade(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	for _, id := range []string{"A", "B", "C"} {
		pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path) VALUES ($1, $1, 'pending', $2)`, id, dir)
	}
	pool.Exec(ctx, `INSERT INTO task_deps (task_id, depends_on) VALUES ('B','A'),('C','B')`)
	// A starts ready.
	pool.Exec(ctx, `UPDATE tasks SET status='ready' WHERE id='A'`)

	ensureAgent := func(_ context.Context, role, taskID string) (string, error) {
		agentID := "impl-" + taskID
		_, err := pool.Exec(ctx, `
			INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
			VALUES ($1, 'maquinista', $1, $2, 'working', $3)
		`, agentID, taskID, role)
		return agentID, err
	}
	cfg := Config{EnsureAgent: ensureAgent}

	// Dispatch A.
	if ok, err := DispatchOne(ctx, pool, cfg); err != nil || !ok {
		t.Fatalf("A: ok=%v err=%v", ok, err)
	}
	// Complete A (simulating merge).
	pool.Exec(ctx, `UPDATE agents SET status='dead' WHERE task_id='A'`)
	pool.Exec(ctx, `UPDATE tasks SET status='done', done_at=NOW() WHERE id='A'`)

	// B should have been promoted to 'ready' by the refresh_ready_tasks trigger.
	var bStatus string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='B'`).Scan(&bStatus)
	if bStatus != "ready" {
		t.Fatalf("B status=%q, want ready", bStatus)
	}

	// Dispatch B.
	if ok, _ := DispatchOne(ctx, pool, cfg); !ok {
		t.Fatal("B not dispatched")
	}

	// Complete B.
	pool.Exec(ctx, `UPDATE agents SET status='dead' WHERE task_id='B'`)
	pool.Exec(ctx, `UPDATE tasks SET status='done', done_at=NOW() WHERE id='B'`)

	var cStatus string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='C'`).Scan(&cStatus)
	if cStatus != "ready" {
		t.Errorf("C status=%q, want ready", cStatus)
	}

	// Dispatch C.
	if ok, _ := DispatchOne(ctx, pool, cfg); !ok {
		t.Error("C not dispatched")
	}
}

func TestHealMissingInbox(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	pool.Exec(ctx, `INSERT INTO tasks (id, title, status) VALUES ('T', 'x', 'claimed')`)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
		VALUES ('impl-T', 'maquinista', 'impl-T', 'T', 'working', 'implementor')
	`)
	// No inbox row — simulates crash after ensure_agent, before enqueue.

	healed, err := HealMissingInbox(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 1 {
		t.Errorf("healed=%d, want 1", healed)
	}

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE external_msg_id='task:T'`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1", count)
	}
}

func TestDispatchOne_ConcurrentRacersDispatchOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path) VALUES ('T', 'x', 'ready', $1)`, dir)

	var mu sync.Mutex
	ensured := 0
	cfg := Config{
		EnsureAgent: func(_ context.Context, role, taskID string) (string, error) {
			agentID := "impl-" + taskID
			_, err := pool.Exec(ctx, `
				INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
				VALUES ($1, 'maquinista', $1, $2, 'working', $3)
			`, agentID, taskID, role)
			if err == nil {
				mu.Lock()
				ensured++
				mu.Unlock()
			}
			return agentID, err
		},
	}

	var wg sync.WaitGroup
	successCount := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := DispatchOne(ctx, pool, cfg)
			if ok {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if ensured != 1 {
		t.Errorf("ensured=%d, want 1 (unique-live + SKIP LOCKED)", ensured)
	}
	// Only one of the two callers dispatched successfully; the other
	// hit no-ready-rows after the first won the claim TX.
	if successCount != 1 {
		t.Errorf("successes=%d, want 1", successCount)
	}
}
