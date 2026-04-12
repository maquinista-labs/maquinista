package inboxbridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
		Content:        []byte("{\"type\":\"text\",\"text\":\"" + text + "\"}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestProcessOne_DrivesPtyAndAcks(t *testing.T) {
	pool := setup(t)
	id := enqueue(t, pool, "tg:1", "hello")

	var mu sync.Mutex
	var drives []string
	drive := func(agentID, text string) error {
		mu.Lock()
		drives = append(drives, agentID+"|"+text)
		mu.Unlock()
		return nil
	}

	cfg := DefaultConfig("w1")
	cfg.MaxPerWake = 1
	processed, err := processOne(context.Background(), pool, "alpha", drive, cfg)
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}

	if len(drives) != 1 || drives[0] != "alpha|hello" {
		t.Errorf("drives=%v", drives)
	}

	var status string
	pool.QueryRow(context.Background(), `SELECT status FROM agent_inbox WHERE id=$1`, id).Scan(&status)
	if status != "processed" {
		t.Errorf("status=%q, want processed", status)
	}
}

func TestProcessOne_DriverErrorTriggersFail(t *testing.T) {
	pool := setup(t)
	id := enqueue(t, pool, "tg:2", "boom")

	drive := func(agentID, text string) error { return pgx.ErrNoRows /* any error */ }

	cfg := DefaultConfig("w1")
	_, err := processOne(context.Background(), pool, "alpha", drive, cfg)
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}

	var status string
	var attempts int
	pool.QueryRow(context.Background(),
		`SELECT status, attempts FROM agent_inbox WHERE id=$1`, id).Scan(&status, &attempts)
	if status != "pending" {
		t.Errorf("status=%q, want pending (first retry)", status)
	}
	if attempts != 1 {
		t.Errorf("attempts=%d, want 1", attempts)
	}
}

func TestEnqueueInbox_IdempotentReplayOneTurn(t *testing.T) {
	pool := setup(t)
	// Replay the same update twice.
	enqueue(t, pool, "tg:5", "replay")
	enqueue(t, pool, "tg:5", "replay")

	var count int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_inbox WHERE external_msg_id='tg:5'`).Scan(&count)
	if count != 1 {
		t.Errorf("rows=%d, want 1", count)
	}

	var drives int
	drive := func(agentID, text string) error { drives++; return nil }

	// Drain: only one turn should fire.
	ok, err := processOne(context.Background(), pool, "alpha", drive, DefaultConfig("w1"))
	if err != nil || !ok {
		t.Fatalf("first ok=%v err=%v", ok, err)
	}
	ok, err = processOne(context.Background(), pool, "alpha", drive, DefaultConfig("w1"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if ok {
		t.Error("expected no further work after single turn")
	}
	if drives != 1 {
		t.Errorf("drives=%d, want 1", drives)
	}
}

func TestRun_EndToEnd_NotifyWakesBridge(t *testing.T) {
	pool := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	seen := make(chan string, 4)
	drive := func(agentID, text string) error {
		mu.Lock()
		defer mu.Unlock()
		seen <- text
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- Run(ctx, pool, drive, DefaultConfig("rt")) }()

	// Insert one row — the LISTEN should wake us within a second.
	enqueue(t, pool, "tg:rt1", "wake-up")

	select {
	case text := <-seen:
		if text != "wake-up" {
			t.Errorf("text=%q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge never saw the insert")
	}

	cancel()
	<-done
}
