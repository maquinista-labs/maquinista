package sidecar

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// setupManager creates a test DB, runs migrations, and returns a pool.
// seedAgents inserts agents rows with status='running' and a non-empty
// tmux_window so Manager.Boot/Sync considers them live.
func setupManager(t *testing.T, agentIDs ...string) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	for _, id := range agentIDs {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO agents (id, tmux_session, tmux_window, status)
			 VALUES ($1, 'test', $2, 'running')`, id, id+"-win"); err != nil {
			t.Fatalf("seed agent %s: %v", id, err)
		}
	}
	return pool
}

// enqueueFor inserts one agent_inbox row for agentID.
func enqueueFor(t *testing.T, pool *pgxpool.Pool, agentID, extID, text string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	chat := int64(-1001)
	if _, _, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:        agentID,
		FromKind:       "user",
		OriginChannel:  "telegram",
		OriginUserID:   "u1",
		OriginThreadID: "1",
		OriginChatID:   &chat,
		ExternalMsgID:  extID,
		Content:        []byte(fmt.Sprintf(`{"type":"text","text":%q}`, text)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// countProcessed returns the number of agent_inbox rows in status='processed'
// for agentID.
func countProcessed(t *testing.T, pool *pgxpool.Pool, agentID string) int {
	t.Helper()
	var n int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_inbox WHERE agent_id=$1 AND status='processed'`,
		agentID).Scan(&n)
	return n
}

// waitProcessed polls until agentID has at least want processed rows or times out.
func waitProcessed(t *testing.T, pool *pgxpool.Pool, agentID string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if countProcessed(t, pool, agentID) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("agent %s: only %d processed rows after %s (want %d)",
		agentID, countProcessed(t, pool, agentID), timeout, want)
}

// makeFakeDriver returns a PtyDriverFunc that records calls per agentID
// and is safe for concurrent use.
func makeFakeDriver(drives *sync.Map) func(agentID string) PtyDriver {
	return func(agentID string) PtyDriver {
		return PtyDriverFunc(func(ctx context.Context, text string) error {
			existing, _ := drives.LoadOrStore(agentID, new(atomic.Int32))
			existing.(*atomic.Int32).Add(1)
			return nil
		})
	}
}

// --- Tests ---

// TestManager_Boot_SpawnsPerAgent: Boot with two live agents. Each gets its
// own sidecar and processes its own inbox rows independently.
func TestManager_Boot_SpawnsPerAgent(t *testing.T) {
	pool := setupManager(t, "agent-a", "agent-b")

	enqueueFor(t, pool, "agent-a", "a:1", "hello from a")
	enqueueFor(t, pool, "agent-b", "b:1", "hello from b")

	// Track which agents were driven and via which driver.
	var drives sync.Map
	driverFactory := makeFakeDriver(&drives)

	// Override spawnLocked's driver via a Manager subtype isn't possible
	// directly (makeTmuxDriver is private). Instead, we test at the
	// integration level: the manager routes inbox rows to per-agent sidecars,
	// and here we inject fake drivers by patching via a test-only helper that
	// builds the sidecar differently. Since the Manager's real driver does a
	// DB lookup + tmux call (which would fail in tests), we provide a
	// test-friendly Manager that uses the injected factory.
	mgr := newTestManager(pool, driverFactory)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	n, err := mgr.Boot(ctx)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if n != 2 {
		t.Errorf("Boot returned %d, want 2", n)
	}
	if mgr.ActiveCount() != 2 {
		t.Errorf("ActiveCount=%d, want 2", mgr.ActiveCount())
	}

	waitProcessed(t, pool, "agent-a", 1, 10*time.Second)
	waitProcessed(t, pool, "agent-b", 1, 10*time.Second)
}

// TestManager_Spawn_AfterBoot: Spawn a sidecar for a new agent after Boot.
func TestManager_Spawn_AfterBoot(t *testing.T) {
	pool := setupManager(t, "agent-x")

	var drives sync.Map
	mgr := newTestManager(pool, makeFakeDriver(&drives))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Boot with no live agents (agent-x has status='running' but we
	// insert agent-y after the fact).
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window, status)
		 VALUES ('agent-y','test','agent-y-win','running')`); err != nil {
		t.Fatal(err)
	}
	enqueueFor(t, pool, "agent-y", "y:1", "late message")

	// Boot only picks up existing agents at call time. Spawn adds agent-y.
	mgr.parentCtx = ctx
	mgr.Spawn("agent-y")

	if mgr.ActiveCount() != 1 {
		t.Errorf("after Spawn ActiveCount=%d, want 1", mgr.ActiveCount())
	}

	waitProcessed(t, pool, "agent-y", 1, 10*time.Second)
}

// TestManager_Teardown_StopsSidecar: Teardown stops a running sidecar.
func TestManager_Teardown_StopsSidecar(t *testing.T) {
	pool := setupManager(t, "agent-t")

	var drives sync.Map
	mgr := newTestManager(pool, makeFakeDriver(&drives))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr.parentCtx = ctx
	mgr.Spawn("agent-t")

	if mgr.ActiveCount() != 1 {
		t.Fatalf("before teardown ActiveCount=%d, want 1", mgr.ActiveCount())
	}

	mgr.Teardown("agent-t")

	// Give the goroutine a moment to clean up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.ActiveCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.ActiveCount() != 0 {
		t.Errorf("after Teardown ActiveCount=%d, want 0", mgr.ActiveCount())
	}
}

// TestManager_Sync_PicksUpNewAgent: Sync after a new agent comes online
// picks it up without requiring a Spawn call.
func TestManager_Sync_PicksUpNewAgent(t *testing.T) {
	pool := setupManager(t) // no agents yet

	var drives sync.Map
	mgr := newTestManager(pool, makeFakeDriver(&drives))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if n, err := mgr.Boot(ctx); err != nil || n != 0 {
		t.Fatalf("Boot: n=%d err=%v, want 0 sidecars", n, err)
	}

	// Agent comes online.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window, status)
		 VALUES ('agent-s','test','agent-s-win','running')`); err != nil {
		t.Fatal(err)
	}
	enqueueFor(t, pool, "agent-s", "s:1", "sync message")

	// Sync should discover agent-s and start its sidecar.
	n, err := mgr.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 1 {
		t.Errorf("Sync started %d, want 1", n)
	}

	waitProcessed(t, pool, "agent-s", 1, 10*time.Second)
}

// TestManager_OnClaim_Forwarded: OnClaim is forwarded from the manager to
// each sidecar and fires with the correct agentID and inboxID.
func TestManager_OnClaim_Forwarded(t *testing.T) {
	pool := setupManager(t, "agent-c")
	enqueueFor(t, pool, "agent-c", "c:1", "claim test")

	type claimEvent struct{ agentID, inboxID string }
	claims := make(chan claimEvent, 4)
	onClaim := func(agentID, inboxID string) {
		claims <- claimEvent{agentID, inboxID}
	}

	var drives sync.Map
	mgr := newTestManagerWithClaim(pool, makeFakeDriver(&drives), onClaim)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := mgr.Boot(ctx); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	select {
	case ev := <-claims:
		if ev.agentID != "agent-c" {
			t.Errorf("OnClaim agentID=%q, want agent-c", ev.agentID)
		}
		if ev.inboxID == "" {
			t.Error("OnClaim inboxID is empty")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnClaim never fired")
	}
}

// TestManager_TwoAgents_ConcurrentInbox: two agents each with a queued inbox
// row both complete within a short window, proving parallel processing.
func TestManager_TwoAgents_ConcurrentInbox(t *testing.T) {
	pool := setupManager(t, "par-1", "par-2")

	// Each agent has a slow driver (200ms). Serial processing would take
	// ≥400ms; parallel should finish in ~200ms + overhead.
	var drives sync.Map
	slowDriverFactory := func(agentID string) PtyDriver {
		return PtyDriverFunc(func(ctx context.Context, text string) error {
			time.Sleep(200 * time.Millisecond)
			existing, _ := drives.LoadOrStore(agentID, new(atomic.Int32))
			existing.(*atomic.Int32).Add(1)
			return nil
		})
	}

	enqueueFor(t, pool, "par-1", "p1:1", "slow 1")
	enqueueFor(t, pool, "par-2", "p2:1", "slow 2")

	mgr := newTestManager(pool, slowDriverFactory)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	if _, err := mgr.Boot(ctx); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	waitProcessed(t, pool, "par-1", 1, 10*time.Second)
	waitProcessed(t, pool, "par-2", 1, 10*time.Second)

	elapsed := time.Since(start)
	// Allow up to 3× the single-agent time to account for scheduling jitter.
	if elapsed > 600*time.Millisecond {
		t.Errorf("parallel processing took %s; expected ~200ms (serial would be ~400ms)", elapsed)
	}
}

// --- Test-only Manager helpers ---

// testManager embeds Manager but overrides spawnLocked to use injected drivers
// instead of the production makeTmuxDriver.
type testManager struct {
	Manager
	driverFactory func(agentID string) PtyDriver
}

func newTestManager(pool *pgxpool.Pool, df func(string) PtyDriver) *testManager {
	return newTestManagerWithClaim(pool, df, nil)
}

func newTestManagerWithClaim(pool *pgxpool.Pool, df func(string) PtyDriver, onClaim func(string, string)) *testManager {
	m := &testManager{
		Manager: Manager{
			pool:    pool,
			session: "test",
			onClaim: onClaim,
			running: make(map[string]*managedSidecar),
		},
		driverFactory: df,
	}
	return m
}

// Boot overrides Manager.Boot to store parentCtx and call our Sync.
func (m *testManager) Boot(ctx context.Context) (int, error) {
	m.mu.Lock()
	m.parentCtx = ctx
	m.mu.Unlock()
	return m.Sync(ctx)
}

// Sync overrides Manager.Sync to use our spawnLocked.
func (m *testManager) Sync(ctx context.Context) (int, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id FROM agents
		WHERE status IN ('running','idle','working')
		  AND tmux_window <> ''
		  AND stop_requested = FALSE
	`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()
	started := 0
	for _, id := range ids {
		if _, exists := m.running[id]; !exists {
			m.spawnTestSidecar(id)
			started++
		}
	}
	return started, nil
}

// Spawn overrides Manager.Spawn.
func (m *testManager) Spawn(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.running[agentID]; exists {
		return
	}
	m.spawnTestSidecar(agentID)
}

func (m *testManager) spawnTestSidecar(agentID string) {
	ctx, cancel := context.WithCancel(m.parentCtx)
	done := make(chan struct{})

	cfg := DefaultConfig(agentID)
	cfg.OnClaim = m.onClaim

	driver := m.driverFactory(agentID)
	s := New(m.pool, cfg, driver, noopTailer{})

	go func() {
		defer close(done)
		defer func() {
			m.mu.Lock()
			delete(m.running, agentID)
			m.mu.Unlock()
		}()
		if err := s.Run(ctx); err != nil && ctx.Err() == nil {
			// Ignore expected context cancel.
		}
	}()

	m.running[agentID] = &managedSidecar{cancel: cancel, done: done}
}
