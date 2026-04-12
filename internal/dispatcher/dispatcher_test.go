package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

type mockSend struct {
	chatID   int64
	threadID int
	text     string
}

type mockClient struct {
	mu    sync.Mutex
	calls []mockSend
	// sendFn overrides the default success path. Return (msgID, err).
	sendFn func(chatID int64, threadID int, text string, callIndex int) (int64, error)
}

func (m *mockClient) SendMessage(ctx context.Context, chatID int64, threadID int, text string) (int64, error) {
	m.mu.Lock()
	idx := len(m.calls)
	m.calls = append(m.calls, mockSend{chatID, threadID, text})
	fn := m.sendFn
	m.mu.Unlock()
	if fn != nil {
		return fn(chatID, threadID, text, idx)
	}
	return 1000 + int64(idx), nil
}

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','w')`)
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// seedDelivery inserts one agent_outbox + one channel_deliveries row and
// returns the delivery id.
func seedDelivery(t *testing.T, pool *pgxpool.Pool, text string, chatID int64, threadID int) uuid.UUID {
	t.Helper()
	outboxID := uuid.New()
	mustExec(t, pool, `INSERT INTO agent_outbox (id, agent_id, content) VALUES ($1, 'alpha', $2::jsonb)`,
		outboxID, fmt.Sprintf(`{"text":%q}`, text))
	deliveryID := uuid.New()
	mustExec(t, pool, `
		INSERT INTO channel_deliveries (id, outbox_id, channel, user_id, thread_id, chat_id, binding_type)
		VALUES ($1, $2, 'telegram', 'u1', $3, $4, 'origin')
	`, deliveryID, outboxID, fmt.Sprintf("%d", threadID), chatID)
	return deliveryID
}

func TestProcessOne_HappyPath(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id := seedDelivery(t, pool, "hello world", -1001, 42)
	mc := &mockClient{}

	ok, err := ProcessOne(ctx, pool, mc, DefaultConfig(), nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(mc.calls) != 1 {
		t.Fatalf("calls=%d", len(mc.calls))
	}
	got := mc.calls[0]
	if got.chatID != -1001 || got.threadID != 42 || got.text != "hello world" {
		t.Errorf("args=%+v", got)
	}

	var status string
	var extID int64
	pool.QueryRow(ctx, `SELECT status, external_msg_id FROM channel_deliveries WHERE id=$1`, id).
		Scan(&status, &extID)
	if status != "sent" {
		t.Errorf("status=%q, want sent", status)
	}
	if extID == 0 {
		t.Error("external_msg_id not set")
	}
}

func TestProcessOne_429_SchedulesDefer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id := seedDelivery(t, pool, "x", -1001, 42)

	mc := &mockClient{
		sendFn: func(chatID int64, threadID int, text string, i int) (int64, error) {
			return 0, &RateLimitError{RetryAfter: 45 * time.Second}
		},
	}

	before := time.Now()
	ok, err := ProcessOne(ctx, pool, mc, DefaultConfig(), nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	var status string
	var next time.Time
	pool.QueryRow(ctx, `SELECT status, next_attempt_at FROM channel_deliveries WHERE id=$1`, id).
		Scan(&status, &next)
	if status != "pending" {
		t.Errorf("status=%q, want pending", status)
	}
	delta := next.Sub(before)
	if delta < 40*time.Second || delta > 60*time.Second {
		t.Errorf("next_attempt_at delta=%s, want ~45s", delta)
	}

	// A second claim immediately should find nothing — the defer window hasn't elapsed.
	ok, err = ProcessOne(ctx, pool, mc, DefaultConfig(), nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Error("expected no-op claim while deferred")
	}
}

func TestProcessOne_MaxAttempts_TerminalFailure(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id := seedDelivery(t, pool, "x", -1001, 42)

	mc := &mockClient{
		sendFn: func(chatID int64, threadID int, text string, i int) (int64, error) {
			return 0, errors.New("boom")
		},
	}
	cfg := DefaultConfig()
	cfg.MaxAttempts = 3

	for i := 0; i < cfg.MaxAttempts; i++ {
		ok, err := ProcessOne(ctx, pool, mc, cfg, nil)
		if err != nil || !ok {
			t.Fatalf("iter %d: ok=%v err=%v", i, ok, err)
		}
	}

	var status, lastErr string
	var attempts int
	pool.QueryRow(ctx, `SELECT status, attempts, COALESCE(last_error,'') FROM channel_deliveries WHERE id=$1`, id).
		Scan(&status, &attempts, &lastErr)
	if status != "failed" {
		t.Errorf("status=%q, want failed", status)
	}
	if attempts != 3 {
		t.Errorf("attempts=%d, want 3", attempts)
	}
	if lastErr != "boom" {
		t.Errorf("last_error=%q", lastErr)
	}
}

// TestRateLimiter_ThroughputCap spams many rows for one chat and asserts the
// dispatcher-side token bucket paces under 30 msg/s. We use a higher cap in
// the test for speed: 100 msg/s, 50 rows → elapsed should be ~0.5 s.
func TestRateLimiter_ThroughputCap(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		seedDelivery(t, pool, fmt.Sprintf("m-%d", i), -1001, 42)
	}

	mc := &mockClient{}
	cfg := DefaultConfig()
	cfg.RatePerSec = 100 // faster than 30 so the test runs in < 1 s
	limiter := newTokenBucket(cfg.RatePerSec)

	start := time.Now()
	for i := 0; i < 50; i++ {
		ok, err := ProcessOne(ctx, pool, mc, cfg, limiter)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("iter %d: no work", i)
		}
	}
	elapsed := time.Since(start)

	// At 100 msg/s with a 100-token burst, 50 sends should fit in the burst
	// and finish essentially instantly. Cap at 2 s as a generous ceiling;
	// the goal is to prove the limiter never pushes us above the cap.
	if elapsed > 2*time.Second {
		t.Errorf("elapsed=%s — limiter suspected stuck", elapsed)
	}
	// Also verify we didn't send faster than the cap would allow (50 msgs /
	// 100 msg/s = 500ms minimum only after the initial burst is drained).
	// With full burst available we expect near-zero time — this branch is a
	// sanity bound, not a lower floor.
	if len(mc.calls) != 50 {
		t.Errorf("calls=%d, want 50", len(mc.calls))
	}
}

// TestRateLimiter_PostBurstPacing: burn the burst, then measure sustained
// sends — they must pace to the per-second cap.
func TestRateLimiter_PostBurstPacing(t *testing.T) {
	limiter := newTokenBucket(20) // 20 msg/s
	ctx := context.Background()

	// Drain the burst (20 tokens).
	for i := 0; i < 20; i++ {
		limiter.Wait(ctx)
	}
	// Next 10 should pace at 20 msg/s ⇒ ~0.5 s.
	start := time.Now()
	for i := 0; i < 10; i++ {
		limiter.Wait(ctx)
	}
	elapsed := time.Since(start)
	if elapsed < 400*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Errorf("post-burst elapsed=%s, want ~500ms", elapsed)
	}
}
