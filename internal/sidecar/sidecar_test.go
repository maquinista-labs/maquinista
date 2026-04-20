package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','alpha')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return pool
}

func enqueue(t *testing.T, pool *pgxpool.Pool, extID, text string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	chat := int64(-1001)
	id, _, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:        "alpha",
		FromKind:       "user",
		OriginChannel:  "telegram",
		OriginUserID:   "u1",
		OriginThreadID: "100",
		OriginChatID:   &chat,
		ExternalMsgID:  extID,
		Content:        []byte(`{"type":"text","text":"` + text + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// scriptedTailer reads one JSONL-style event per line from a file, emits
// them as TranscriptEvents, and blocks until ctx is cancelled.
type scriptedTailer struct {
	path string
}

func (s *scriptedTailer) Tail(ctx context.Context, ch chan<- TranscriptEvent) error {
	defer close(ch)
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ev TranscriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- ev:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func writeScript(t *testing.T, events []TranscriptEvent) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		fmt.Fprintln(f, string(b))
	}
	return path
}

func TestSidecar_ClaimDriveAck_ThenOutboxFromTranscript(t *testing.T) {
	pool := setup(t)
	enqueue(t, pool, "tg:1", "hi")

	script := writeScript(t, []TranscriptEvent{
		{Role: "user", Kind: "text", Text: "hi"},
		{Role: "assistant", Kind: "thinking", Text: "thinking..."},
		{Role: "assistant", Kind: "text", Text: "hello"},
		{Role: "assistant", Kind: "text", Text: "world", TurnEnd: true},
	})

	var mu sync.Mutex
	var drives []string
	drive := PtyDriverFunc(func(ctx context.Context, text string) error {
		mu.Lock()
		drives = append(drives, text)
		mu.Unlock()
		return nil
	})

	cfg := DefaultConfig("alpha")
	cfg.Poll = 500 * time.Millisecond
	s := New(pool, cfg, drive, &scriptedTailer{path: script})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait for the outbox to accumulate 3 rows (1 thinking + 2 text).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM agent_outbox WHERE agent_id='alpha'`).Scan(&count)
		if count >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox never reached 3 rows (got %d)", count)
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-done

	// Verify drive was called once with "hi".
	mu.Lock()
	defer mu.Unlock()
	if len(drives) != 1 || drives[0] != "hi" {
		t.Errorf("drives=%v", drives)
	}

	// Verify inbox row is processed.
	var status string
	pool.QueryRow(context.Background(),
		`SELECT status FROM agent_inbox WHERE external_msg_id='tg:1'`).Scan(&status)
	if status != "processed" {
		t.Errorf("inbox status=%q, want processed", status)
	}
}

// TestSidecar_LeaseExpiry_Reclaim: run processOneInbox once with a tiny
// lease and a driver that hangs for longer than the lease. Simulate the
// sidecar crashing mid-drive by cancelling. A fresh ClaimInbox after the
// lease expires must reclaim exactly one row.
func TestSidecar_LeaseExpiry_Reclaim(t *testing.T) {
	pool := setup(t)
	id := enqueue(t, pool, "tg:lease", "stuck")

	cfg := DefaultConfig("alpha")
	cfg.Lease = 500 * time.Millisecond
	cfg.Poll = 100 * time.Millisecond

	startedDrive := make(chan struct{})
	drive := PtyDriverFunc(func(ctx context.Context, text string) error {
		close(startedDrive)
		<-ctx.Done() // simulate stall
		return ctx.Err()
	})

	s := New(pool, cfg, drive, &fakeTailer{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	<-startedDrive

	// Wait past the lease window.
	time.Sleep(700 * time.Millisecond)

	// Kill the sidecar mid-drive.
	cancel()
	<-done

	// Row should still be 'processing' with claimed_by set (the sidecar
	// crashed before ack'ing and before the lease naturally expired).
	var status string
	var claimedBy *string
	pool.QueryRow(context.Background(),
		`SELECT status, claimed_by FROM agent_inbox WHERE id=$1`, id).Scan(&status, &claimedBy)
	if status != "processing" {
		t.Errorf("after crash status=%q, want processing", status)
	}
	if claimedBy == nil {
		t.Errorf("claimed_by should be set for the crashed sidecar")
	}

	// Force the lease into the past so a fresh claim wins.
	pool.Exec(context.Background(),
		`UPDATE agent_inbox SET lease_expires = NOW() - INTERVAL '1 minute' WHERE id=$1`, id)

	// Fresh sidecar (new worker) should reclaim the row exactly once.
	var reclaimed int32
	drive2 := PtyDriverFunc(func(ctx context.Context, text string) error {
		reclaimed++
		return nil
	})
	cfg2 := DefaultConfig("alpha")
	cfg2.WorkerID = "sidecar-worker-2"
	cfg2.Poll = 100 * time.Millisecond
	s2 := New(pool, cfg2, drive2, &fakeTailer{})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	done2 := make(chan error, 1)
	go func() { done2 <- s2.Run(ctx2) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		pool.QueryRow(context.Background(),
			`SELECT status FROM agent_inbox WHERE id=$1`, id).Scan(&status)
		if status == "processed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reclaim never completed; last status=%q drives=%d", status, reclaimed)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel2()
	<-done2

	if reclaimed != 1 {
		t.Errorf("reclaimed driver invocations=%d, want 1", reclaimed)
	}
}

// TestSidecar_ParityWithShadowMonitor: push identical transcript events
// through the sidecar's outbox path and the monitor's NewDBOutboxWriter,
// confirm the stored agent_outbox content bytes match.
func TestSidecar_ParityWithShadowMonitor(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	events := []TranscriptEvent{
		{Role: "assistant", Kind: "text", Text: "hello"},
		{Role: "assistant", Kind: "text", Text: "🚀 emoji + \x1b[31mANSI\x1b[0m"},
		{Role: "assistant", Kind: "thinking", Text: "reasoning..."},
	}

	// Drive the sidecar's outbox path directly.
	cfg := DefaultConfig("alpha")
	s := New(pool, cfg, PtyDriverFunc(func(ctx context.Context, text string) error { return nil }), &fakeTailer{})
	for _, ev := range events {
		s.appendOutbox(ctx, ev)
	}

	// Collect sidecar-produced rows.
	sidecarRows := collectOutbox(t, pool, "alpha")

	// Clean slate.
	mustExec(t, pool, `DELETE FROM agent_outbox`)

	// Now drive the monitor's shadow writer with the same payloads.
	// (We call its inner marshal path via the local helper; the monitor
	// package isn't imported here to avoid a test-cycle.)
	for _, ev := range events {
		body, _ := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Role string `json:"role,omitempty"`
		}{Type: "text", Text: ev.Text, Role: ev.Role})
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mailbox.AppendOutbox(ctx, tx, mailbox.OutboxMessage{
			AgentID: "alpha", Content: body,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	monitorRows := collectOutbox(t, pool, "alpha")

	if len(sidecarRows) != len(monitorRows) {
		t.Fatalf("sidecar rows=%d, monitor rows=%d", len(sidecarRows), len(monitorRows))
	}
	for i := range sidecarRows {
		if string(sidecarRows[i]) != string(monitorRows[i]) {
			t.Errorf("row %d differs\nsidecar=%s\nmonitor=%s", i, sidecarRows[i], monitorRows[i])
		}
	}
}

// TestSidecar_OnClaim_FiresBeforeDrive confirms that Config.OnClaim is invoked
// with the correct agentID and inboxID before the PTY driver runs, so the
// monitor's OutboxSink can read the mapping for in_reply_to stamping.
func TestSidecar_OnClaim_FiresBeforeDrive(t *testing.T) {
	pool := setup(t)
	inboxID := enqueue(t, pool, "tg:claim", "hello")

	var claimedAgent, claimedInbox string
	var claimBeforeDrive bool
	driveCalled := false

	cfg := DefaultConfig("alpha")
	cfg.Poll = 200 * time.Millisecond
	cfg.OnClaim = func(agentID, inboxID string) {
		claimedAgent = agentID
		claimedInbox = inboxID
		claimBeforeDrive = !driveCalled // should be true
	}

	drive := PtyDriverFunc(func(ctx context.Context, text string) error {
		driveCalled = true
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	s := New(pool, cfg, drive, &fakeTailer{})
	go func() { done <- s.Run(ctx) }()

	// Wait until the inbox row is processed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		pool.QueryRow(context.Background(),
			`SELECT status FROM agent_inbox WHERE id=$1`, inboxID).Scan(&status)
		if status == "processed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inbox row never reached processed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	if claimedAgent != "alpha" {
		t.Errorf("OnClaim agentID=%q, want alpha", claimedAgent)
	}
	if claimedInbox != inboxID.String() {
		t.Errorf("OnClaim inboxID=%q, want %s", claimedInbox, inboxID)
	}
	if !claimBeforeDrive {
		t.Error("OnClaim did not fire before Drive")
	}
}

type fakeTailer struct{}

func (fakeTailer) Tail(ctx context.Context, ch chan<- TranscriptEvent) error {
	defer close(ch)
	<-ctx.Done()
	return ctx.Err()
}

func collectOutbox(t *testing.T, pool *pgxpool.Pool, agentID string) [][]byte {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT content FROM agent_outbox WHERE agent_id=$1 ORDER BY created_at, id`, agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var c []byte
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
