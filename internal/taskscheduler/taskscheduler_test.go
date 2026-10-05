package taskscheduler

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// Regression (MAQ-18): a ready task whose only agent row is 'stopped'
// (frozen SpawnFresh pre-registration, sidecar vanished-window mark, or
// `maquinista stop` park) must be claimed, and the stale row must be
// flipped to 'dead' so uq_agents_task_live frees the slot for the fresh
// implementor. Before the fix the task was silently skipped forever.
func TestDispatchOne_ClaimsTaskWithStoppedAgentRow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path) VALUES ('T', 'x', 'ready', $1)`, dir)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, stop_requested, role)
		VALUES ('impl-old', 'maquinista', 'impl-old', 'T', 'stopped', TRUE, 'implementor')
	`)

	ensured := false
	cfg := Config{
		EnsureAgent: func(_ context.Context, role, taskID string) (string, error) {
			ensured = true
			_, err := pool.Exec(ctx, `
				INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
				VALUES ($1, 'maquinista', $1, $2, 'working', $3)
			`, "impl-"+taskID, taskID, role)
			return "impl-" + taskID, err
		},
	}

	ok, err := DispatchOne(ctx, pool, cfg)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !ensured {
		t.Error("EnsureAgent never called — stopped agent row still blocks the claim")
	}

	// The stale stopped row must be released (dead), not left holding the
	// unique-live slot.
	var oldStatus string
	pool.QueryRow(ctx, `SELECT status FROM agents WHERE id='impl-old'`).Scan(&oldStatus)
	if oldStatus != "dead" {
		t.Errorf("stale agent status=%q, want dead", oldStatus)
	}
}

// MAQ-18 acceptance 2: the dashboard's "stopped + empty tmux_window =
// needs provisioning" state (role='user', task_id NULL) is out of the
// scheduler's reach — claiming a task must never flip such a row.
func TestDispatchOne_DoesNotTouchStoppedUserAgents(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path) VALUES ('T', 'x', 'ready', $1)`, dir)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status, stop_requested)
		VALUES ('dash-user', 'maquinista', '', 'user', NULL, 'stopped', FALSE)
	`)

	cfg := Config{EnsureAgent: func(_ context.Context, role, taskID string) (string, error) {
		_, err := pool.Exec(ctx, `
			INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
			VALUES ($1, 'maquinista', $1, $2, 'working', $3)
		`, "impl-"+taskID, taskID, role)
		return "impl-" + taskID, err
	}}
	if ok, err := DispatchOne(ctx, pool, cfg); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	var status, window string
	pool.QueryRow(ctx, `SELECT status, tmux_window FROM agents WHERE id='dash-user'`).Scan(&status, &window)
	if status != "stopped" || window != "" {
		t.Errorf("dashboard agent mutated: status=%q window=%q, want stopped/empty (reconcile would lose it)", status, window)
	}
}

// MAQ-18 acceptance 3: a ready task held by a LIVE agent must be skipped
// (correct behavior) but reported — one journal line naming the blocking
// agent + status, instead of the old silence.
func TestLogBlockedReadyTasks(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path) VALUES ('T', 'x', 'ready', $1)`, dir)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
		VALUES ('impl-live', 'maquinista', 'impl-live', 'T', 'working', 'implementor')
	`)

	cfg := Config{EnsureAgent: func(_ context.Context, _, _ string) (string, error) {
		t.Error("EnsureAgent must not run for a task held by a live agent")
		return "", nil
	}}
	if ok, err := DispatchOne(ctx, pool, cfg); err != nil || ok {
		t.Fatalf("live agent must block the claim: ok=%v err=%v", ok, err)
	}

	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	n, err := LogBlockedReadyTasks(ctx, pool)
	log.SetOutput(os.Stderr) // nil would leave the default logger panicking on the next log call
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("blocked tasks=%d, want 1", n)
	}
	out := logBuf.String()
	for _, want := range []string{"T", "impl-live", "working"} {
		if !strings.Contains(out, want) {
			t.Errorf("journal line missing %q: %q", want, out)
		}
	}
}

// MAQ-18 reaper: a 'claimed' task whose agent row went 'stopped'
// mid-flight (pane vanished → sidecar mark) is released back to 'ready'
// once the claim is stale. Fresh claims and claims with live agents are
// left alone — the bound protects the SpawnFresh stopped→running window.
func TestReapStaleClaims(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	dir := t.TempDir()
	// Stale claim with a dead-paned (stopped) agent → reaped.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path, claimed_at, claimed_by)
		VALUES ('STALE', 'x', 'claimed', $1, NOW() - INTERVAL '10 minutes', '@impl-old')`, dir)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, stop_requested, role)
		VALUES ('impl-old', 'maquinista', 'impl-old', 'STALE', 'stopped', TRUE, 'implementor')
	`)
	// Fresh claim (agent row still in SpawnFresh's stopped phase) → kept.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path, claimed_at)
		VALUES ('FRESH', 'x', 'claimed', $1, NOW())`, dir)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
		VALUES ('impl-new', 'maquinista', 'impl-new', 'FRESH', 'stopped', 'implementor')
	`)
	// Stale claim whose agent is genuinely live → kept.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, worktree_path, claimed_at)
		VALUES ('LIVE', 'x', 'claimed', $1, NOW() - INTERVAL '10 minutes')`, dir)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
		VALUES ('impl-live', 'maquinista', 'impl-live', 'LIVE', 'working', 'implementor')
	`)

	reaped, err := ReapStaleClaims(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped=%d, want 1", reaped)
	}

	var status string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='STALE'`).Scan(&status)
	if status != "ready" {
		t.Errorf("STALE status=%q, want ready", status)
	}
	var claimedBy *string
	pool.QueryRow(ctx, `SELECT claimed_by FROM tasks WHERE id='STALE'`).Scan(&claimedBy)
	if claimedBy != nil {
		t.Errorf("STALE claimed_by=%v, want NULL", *claimedBy)
	}
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='FRESH'`).Scan(&status)
	if status != "claimed" {
		t.Errorf("FRESH status=%q, want claimed (fresh spawn window)", status)
	}
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='LIVE'`).Scan(&status)
	if status != "claimed" {
		t.Errorf("LIVE status=%q, want claimed (agent still working)", status)
	}

	// End-to-end: the reaped task is immediately re-claimable despite the
	// stopped row, and that claim releases the row (acceptance 1).
	cfg := Config{EnsureAgent: func(_ context.Context, role, taskID string) (string, error) {
		_, err := pool.Exec(ctx, `
			INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
			VALUES ($1, 'maquinista', $1, $2, 'working', $3)
		`, "impl-"+taskID, taskID, role)
		return "impl-" + taskID, err
	}}
	if ok, err := DispatchOne(ctx, pool, cfg); err != nil || !ok {
		t.Fatalf("re-dispatch after reap: ok=%v err=%v", ok, err)
	}
}

// syncBuffer is a thread-safe-ish log sink for tests.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Regression (EX-07 round 2): a claimed task whose inbox prompt is owned by
// a DEAD previous agent (stale processed row from attempt 1) must heal —
// enqueueWorkOnTask upserts the same-key row and repoints it at the live
// agent. Without this the fresh worker sits at an idle prompt forever.
func TestHealMissingInbox_RepointsStaleRow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	pool.Exec(ctx, `INSERT INTO tasks (id, title, status) VALUES ('T', 'x', 'claimed')`)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
		VALUES ('impl-old', 'maquinista', 'impl-old', 'T', 'dead', 'implementor')
	`)
	pool.Exec(ctx, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status, role)
		VALUES ('impl-T', 'maquinista', 'impl-T', 'T', 'working', 'implementor')
	`)
	// Stale prompt row owned by the dead attempt-1 agent.
	pool.Exec(ctx, `
		INSERT INTO agent_inbox (agent_id, from_kind, from_id, origin_channel, external_msg_id, content, status, attempts)
		VALUES ('impl-old', 'system', 'task-scheduler', 'task', 'task:T', '{"type":"task"}', 'processed', 1)
	`)

	healed, err := HealMissingInbox(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if healed != 1 {
		t.Errorf("healed=%d, want 1", healed)
	}

	var agentID, status string
	pool.QueryRow(ctx, `SELECT agent_id, status FROM agent_inbox WHERE origin_channel='task' AND external_msg_id='task:T'`).Scan(&agentID, &status)
	if agentID != "impl-T" || status != "pending" {
		t.Errorf("agent=%q status=%q, want impl-T/pending", agentID, status)
	}
}

// --- MAQ-13: unspawnable (worktree-less) tasks park needs-human exactly once.

func TestDispatchOne_ParksWithoutWorktree(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Ready task with NO worktree_path — structurally unspawnable.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status) VALUES ('T', 'x', 'ready')`)

	ensureCalled := false
	cfg := Config{
		EnsureAgent: func(ctx context.Context, role, taskID string) (string, error) {
			ensureCalled = true
			return "impl-" + taskID, nil
		},
	}

	ok, err := DispatchOne(ctx, pool, cfg)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ensureCalled {
		t.Error("EnsureAgent must not be called for a worktree-less task")
	}

	var status string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='T'`).Scan(&status)
	if status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval (parked needs-human)", status)
	}
	var notes int
	pool.QueryRow(ctx, `SELECT count(*) FROM task_context WHERE task_id='T' AND kind='verdict'`).Scan(&notes)
	if notes != 1 {
		t.Errorf("task_context notes = %d, want exactly 1", notes)
	}
	var agents, inbox int
	pool.QueryRow(ctx, `SELECT count(*) FROM agents WHERE task_id='T'`).Scan(&agents)
	pool.QueryRow(ctx, `SELECT count(*) FROM agent_inbox WHERE external_msg_id='task:T'`).Scan(&inbox)
	if agents != 0 || inbox != 0 {
		t.Errorf("agents=%d inbox=%d, want 0/0 (no half-spawned pane)", agents, inbox)
	}
	// Pipeline topic ping: exactly one outbox row on the synthetic notifier.
	var pings int
	pool.QueryRow(ctx, `SELECT count(*) FROM agent_outbox WHERE agent_id='pipeline' AND content->>'text' LIKE '%T%'`).Scan(&pings)
	if pings != 1 {
		t.Errorf("pipeline pings = %d, want 1", pings)
	}
}

func TestDispatchOne_WorktreelessTaskNotReclaimed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	pool.Exec(ctx, `INSERT INTO tasks (id, title, status) VALUES ('T', 'x', 'ready')`)
	cfg := Config{EnsureAgent: func(ctx context.Context, role, taskID string) (string, error) {
		return "impl-" + taskID, nil
	}}

	// First dispatch parks it; a second tick must find nothing (no loop).
	if ok, err := DispatchOne(ctx, pool, cfg); err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	if ok, _ := DispatchOne(ctx, pool, cfg); ok {
		t.Error("parked task must not be re-claimed")
	}
}

func TestParkUnspawnable_GraceAndOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Old claimed task, no worktree, no live agent → parked.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, claimed_at) VALUES ('OLD', 'x', 'claimed', NOW() - interval '20 minutes')`)
	// Young claimed task → still inside grace, left alone.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, claimed_at) VALUES ('NEW', 'x', 'claimed', NOW())`)
	// Old claimed task WITH worktree → has nothing to do with this pass.
	pool.Exec(ctx, `INSERT INTO tasks (id, title, status, claimed_at, worktree_path) VALUES ('WT', 'x', 'claimed', NOW() - interval '20 minutes', $1)`, t.TempDir())

	parked, err := ParkUnspawnable(ctx, pool, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if parked != 1 {
		t.Fatalf("parked=%d, want 1", parked)
	}
	var status string
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='OLD'`).Scan(&status)
	if status != "pending_approval" {
		t.Errorf("OLD status=%q, want pending_approval", status)
	}
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='NEW'`).Scan(&status)
	if status != "claimed" {
		t.Errorf("NEW status=%q, want claimed (inside grace)", status)
	}
	pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id='WT'`).Scan(&status)
	if status != "claimed" {
		t.Errorf("WT status=%q, want claimed (has worktree)", status)
	}

	// Exactly once: second pass finds nothing to park.
	if parked, _ := ParkUnspawnable(ctx, pool, 10*time.Minute); parked != 0 {
		t.Errorf("second pass parked=%d, want 0", parked)
	}
}
