package mailbox

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	exec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','w')`)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func withTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestEnqueueInbox_Idempotent(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var first, second uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		id, ok, err := EnqueueInbox(ctx, tx, InboxMessage{
			AgentID:       "alpha",
			FromKind:      "user",
			OriginChannel: "telegram",
			ExternalMsgID: "u-42",
			Content:       []byte(`{"type":"text","text":"hi"}`),
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if !ok {
			t.Fatal("first enqueue should report inserted")
		}
		first = id
	})

	withTx(t, pool, func(tx pgx.Tx) {
		id, ok, err := EnqueueInbox(ctx, tx, InboxMessage{
			AgentID:       "alpha",
			FromKind:      "user",
			OriginChannel: "telegram",
			ExternalMsgID: "u-42",
			Content:       []byte(`{"type":"text","text":"hi-dup"}`),
		})
		if err != nil {
			t.Fatalf("enqueue dup: %v", err)
		}
		if ok {
			t.Error("duplicate should not report inserted")
		}
		second = id
	})

	if first != second {
		t.Errorf("idempotent lookup returned different id: %s vs %s", first, second)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1", count)
	}
}

func TestClaimInbox_SkipLocked_NoDoubleClaim(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Seed 10 pending rows.
	for i := 0; i < 10; i++ {
		withTx(t, pool, func(tx pgx.Tx) {
			_, _, err := EnqueueInbox(ctx, tx, InboxMessage{
				AgentID:       "alpha",
				FromKind:      "user",
				OriginChannel: "telegram",
				ExternalMsgID: fmt.Sprintf("m-%d", i),
				Content:       []byte(`{"type":"text","text":"x"}`),
			})
			if err != nil {
				t.Fatalf("seed enqueue: %v", err)
			}
		})
	}

	var (
		mu    sync.Mutex
		seen  = map[uuid.UUID]int{}
		total int
	)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Errorf("begin: %v", err)
					return
				}
				rows, err := ClaimInbox(ctx, tx, "alpha", fmt.Sprintf("worker-%d", worker), 30*time.Second, 3)
				if err != nil {
					tx.Rollback(ctx)
					t.Errorf("claim: %v", err)
					return
				}
				if len(rows) == 0 {
					tx.Rollback(ctx)
					return
				}
				mu.Lock()
				for _, r := range rows {
					seen[r.ID]++
					total++
				}
				mu.Unlock()
				if err := tx.Commit(ctx); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if total != 10 {
		t.Errorf("total claimed = %d, want 10", total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s claimed %d times", id, n)
		}
	}
}

func TestClaimInbox_LeaseExpiry(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var id uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		got, _, err := EnqueueInbox(ctx, tx, InboxMessage{
			AgentID:       "alpha",
			FromKind:      "user",
			OriginChannel: "telegram",
			ExternalMsgID: "lease-1",
			Content:       []byte(`{"type":"text","text":"x"}`),
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		id = got
	})

	// First claim with a short lease.
	withTx(t, pool, func(tx pgx.Tx) {
		rows, err := ClaimInbox(ctx, tx, "alpha", "w1", 50*time.Millisecond, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("first claim: rows=%d err=%v", len(rows), err)
		}
		if rows[0].ID != id {
			t.Fatalf("claimed wrong id")
		}
	})

	// Simulate stall: roll back nothing, but force the lease past.
	exec(t, pool, `UPDATE agent_inbox SET lease_expires = NOW() - INTERVAL '1 minute' WHERE id=$1`, id)

	var reclaimedID uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		rows, err := ClaimInbox(ctx, tx, "alpha", "w2", time.Minute, 1)
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected reclaim, got %d rows", len(rows))
		}
		reclaimedID = rows[0].ID
		if rows[0].Attempts != 2 {
			t.Errorf("attempts=%d, want 2", rows[0].Attempts)
		}
	})
	if reclaimedID != id {
		t.Errorf("reclaim returned wrong id")
	}
}

func TestAckInbox(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var id uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		got, _, err := EnqueueInbox(ctx, tx, InboxMessage{
			AgentID: "alpha", FromKind: "user",
			OriginChannel: "telegram", ExternalMsgID: "ack-1",
			Content: []byte(`{"type":"text","text":"x"}`),
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		id = got
	})

	withTx(t, pool, func(tx pgx.Tx) {
		if _, err := ClaimInbox(ctx, tx, "alpha", "w", time.Minute, 1); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := AckInbox(ctx, tx, id); err != nil {
			t.Fatalf("ack: %v", err)
		}
	})

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_inbox WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "processed" {
		t.Errorf("status = %q, want processed", status)
	}
}

func TestFailInbox_DeadOnExhaustedAttempts(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var id uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		got, _, err := EnqueueInbox(ctx, tx, InboxMessage{
			AgentID: "alpha", FromKind: "user",
			OriginChannel: "telegram", ExternalMsgID: "fail-1",
			Content: []byte(`{"type":"text","text":"x"}`),
			MaxAttempts: 2,
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		id = got
	})

	// First claim + fail: still retryable.
	withTx(t, pool, func(tx pgx.Tx) {
		if _, err := ClaimInbox(ctx, tx, "alpha", "w", time.Minute, 1); err != nil {
			t.Fatalf("claim1: %v", err)
		}
		if err := FailInbox(ctx, tx, id, "boom"); err != nil {
			t.Fatalf("fail1: %v", err)
		}
	})

	var status string
	pool.QueryRow(ctx, `SELECT status FROM agent_inbox WHERE id=$1`, id).Scan(&status)
	if status != "pending" {
		t.Errorf("after 1st fail status=%q, want pending", status)
	}

	// Second claim bumps attempts to 2 == max, fail drives to 'dead'.
	withTx(t, pool, func(tx pgx.Tx) {
		if _, err := ClaimInbox(ctx, tx, "alpha", "w", time.Minute, 1); err != nil {
			t.Fatalf("claim2: %v", err)
		}
		if err := FailInbox(ctx, tx, id, "boom2"); err != nil {
			t.Fatalf("fail2: %v", err)
		}
	})

	pool.QueryRow(ctx, `SELECT status FROM agent_inbox WHERE id=$1`, id).Scan(&status)
	if status != "dead" {
		t.Errorf("after 2nd fail status=%q, want dead", status)
	}
}

func TestAppendAndClaimOutbox(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var outboxID uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		id, err := AppendOutbox(ctx, tx, OutboxMessage{
			AgentID: "alpha",
			Content: []byte(`{"parts":[{"type":"text","text":"hello"}]}`),
			Mentions: []byte(`[{"agent_id":"beta","text":"@beta do X"}]`),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		outboxID = id
	})

	withTx(t, pool, func(tx pgx.Tx) {
		rows, err := ClaimOutbox(ctx, tx, 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != outboxID {
			t.Fatalf("claim returned %d rows, want 1 of %s", len(rows), outboxID)
		}
		if !bytes.Contains(rows[0].Mentions, []byte("@beta do X")) {
			t.Errorf("mentions = %s", rows[0].Mentions)
		}
	})

	var status string
	pool.QueryRow(ctx, `SELECT status FROM agent_outbox WHERE id=$1`, outboxID).Scan(&status)
	if status != "routing" {
		t.Errorf("status = %q, want routing", status)
	}
}

func TestAckChannelDelivery(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var outboxID, deliveryID uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		id, err := AppendOutbox(ctx, tx, OutboxMessage{
			AgentID: "alpha",
			Content: []byte(`{"parts":[]}`),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		outboxID = id
	})

	deliveryID = uuid.New()
	exec(t, pool, `
		INSERT INTO channel_deliveries (id, outbox_id, channel, user_id, thread_id, chat_id, binding_type)
		VALUES ($1, $2, 'telegram', 'u1', '100', -1001, 'origin')
	`, deliveryID, outboxID)

	withTx(t, pool, func(tx pgx.Tx) {
		if err := AckChannelDelivery(ctx, tx, deliveryID, 777); err != nil {
			t.Fatalf("ack: %v", err)
		}
	})

	var status string
	var externalMsgID int64
	pool.QueryRow(ctx, `SELECT status, external_msg_id FROM channel_deliveries WHERE id=$1`, deliveryID).
		Scan(&status, &externalMsgID)
	if status != "sent" || externalMsgID != 777 {
		t.Errorf("status=%q msg_id=%d", status, externalMsgID)
	}
}

func TestInsertAttachment_InlineAndLargeObject(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var inboxID uuid.UUID
	withTx(t, pool, func(tx pgx.Tx) {
		id, _, err := EnqueueInbox(ctx, tx, InboxMessage{
			AgentID: "alpha", FromKind: "user",
			OriginChannel: "telegram", ExternalMsgID: "att-1",
			Content: []byte(`{"type":"text","text":"x"}`),
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		inboxID = id
	})

	// Inline (small).
	small := bytes.Repeat([]byte{'a'}, 1024)
	withTx(t, pool, func(tx pgx.Tx) {
		if _, err := InsertAttachment(ctx, tx, AttachmentTarget{InboxID: &inboxID}, "a.txt", "text/plain", small); err != nil {
			t.Fatalf("inline: %v", err)
		}
	})

	// Large object (≥ threshold).
	big := bytes.Repeat([]byte{'b'}, LargeObjectThreshold+16)
	withTx(t, pool, func(tx pgx.Tx) {
		if _, err := InsertAttachment(ctx, tx, AttachmentTarget{InboxID: &inboxID}, "b.bin", "application/octet-stream", big); err != nil {
			t.Fatalf("large: %v", err)
		}
	})

	var inlineCount, loCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM message_attachments WHERE inbox_id=$1 AND content IS NOT NULL`, inboxID).Scan(&inlineCount)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM message_attachments WHERE inbox_id=$1 AND large_object_oid IS NOT NULL`, inboxID).Scan(&loCount)
	if inlineCount != 1 {
		t.Errorf("inline rows=%d, want 1", inlineCount)
	}
	if loCount != 1 {
		t.Errorf("large-object rows=%d, want 1", loCount)
	}
}
