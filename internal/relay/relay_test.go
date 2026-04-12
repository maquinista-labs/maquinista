package relay

import (
	"context"
	"testing"

	"github.com/google/uuid"
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
	mustExec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window) VALUES
			('alpha','s','wa'),
			('beta','s','wb'),
			('gamma','s','wg')
	`)
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// TestProcessOne_FanoutAndMentions drives the full relay TX and verifies:
//   - origin fan-out row created from in_reply_to's inbox row
//   - one delivery per owner/observer binding
//   - duplicate (origin == subscriber) collapsed by UNIQUE
//   - two mentions produce two agent_inbox rows with from_kind='agent'
//   - outbox row ends in 'routed'
func TestProcessOne_FanoutAndMentions(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Bindings: alpha is owner in (u1, thread-100), gamma observes that thread,
	// and alpha is also owner in (u2, thread-200) — a separate subscriber.
	mustExec(t, pool, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		VALUES
			(100, 'alpha',  'owner',    'u1', '100', -1001),
			(100, 'gamma',  'observer', 'u1', '100', -1001),
			(200, 'alpha',  'owner',    'u2', '200', -2002)
	`)

	// Incoming inbox row that triggered alpha's response — comes from (u1, 100).
	inboxID := uuid.New()
	mustExec(t, pool, `
		INSERT INTO agent_inbox (id, agent_id, from_kind, origin_channel, origin_user_id, origin_thread_id, origin_chat_id, content)
		VALUES ($1, 'alpha', 'user', 'telegram', 'u1', '100', -1001, '{"type":"text","text":"go"}'::jsonb)
	`, inboxID)

	// Outbox message from alpha, mentioning beta and gamma.
	outboxID := uuid.New()
	mustExec(t, pool, `
		INSERT INTO agent_outbox (id, agent_id, in_reply_to, content)
		VALUES ($1, 'alpha', $2, $3::jsonb)
	`, outboxID, inboxID, `{"text":"done; [@beta: please verify] also [@gamma: FYI]"}`)

	ok, err := ProcessOne(ctx, pool, "w1")
	if err != nil || !ok {
		t.Fatalf("ProcessOne: ok=%v err=%v", ok, err)
	}

	// Outbox now routed.
	var status string
	pool.QueryRow(ctx, `SELECT status FROM agent_outbox WHERE id=$1`, outboxID).Scan(&status)
	if status != "routed" {
		t.Errorf("outbox status=%q, want routed", status)
	}

	// channel_deliveries rows: origin (u1,100) + owner alpha (u1,100 dedup'd) +
	// observer gamma (u1,100 dedup'd by user+thread) + alpha owner (u2,200).
	// UNIQUE (outbox_id, channel, user_id, thread_id) collapses rows sharing
	// the same (u,t). So we expect 2 distinct deliveries by (user,thread):
	//   (u1, 100) — deduped across origin/owner/observer
	//   (u2, 200)
	var dCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM channel_deliveries WHERE outbox_id=$1`, outboxID).Scan(&dCount)
	if dCount != 2 {
		t.Errorf("delivery rows = %d, want 2", dCount)
	}

	// Mentions → agent_inbox rows with from_kind='agent'.
	var mentionBeta, mentionGamma int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE agent_id='beta'  AND from_kind='agent' AND from_id='alpha'`).Scan(&mentionBeta)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE agent_id='gamma' AND from_kind='agent' AND from_id='alpha'`).Scan(&mentionGamma)
	if mentionBeta != 1 {
		t.Errorf("beta inbox rows=%d, want 1", mentionBeta)
	}
	if mentionGamma != 1 {
		t.Errorf("gamma inbox rows=%d, want 1", mentionGamma)
	}
}

// TestProcessOne_CrashMidTx_PreservesPending simulates a mid-TX crash by
// starting a transaction that does the work then rolls back. The outbox row
// should still be 'pending' and the next call processes it exactly once.
func TestProcessOne_CrashMidTx_PreservesPending(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	mustExec(t, pool, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		VALUES (100, 'alpha', 'owner', 'u1', '100', -1001)
	`)

	outboxID := uuid.New()
	mustExec(t, pool, `
		INSERT INTO agent_outbox (id, agent_id, content)
		VALUES ($1, 'alpha', '{"text":"hi"}'::jsonb)
	`, outboxID)

	// Start a transaction, claim the row, do the work, then ROLL BACK.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Claim + all the work inside tx.
	var gotStatus string
	tx.QueryRow(ctx, `
		UPDATE agent_outbox SET status='routing' WHERE id=$1 RETURNING status
	`, outboxID).Scan(&gotStatus)
	if gotStatus != "routing" {
		t.Fatalf("pre-rollback status=%q", gotStatus)
	}
	_ = tx.Rollback(ctx)

	// After rollback, status must still be 'pending'.
	var status string
	pool.QueryRow(ctx, `SELECT status FROM agent_outbox WHERE id=$1`, outboxID).Scan(&status)
	if status != "pending" {
		t.Fatalf("after rollback status=%q, want pending", status)
	}

	// Now ProcessOne should pick it up and route it exactly once.
	ok, err := ProcessOne(ctx, pool, "w1")
	if err != nil || !ok {
		t.Fatalf("ProcessOne: ok=%v err=%v", ok, err)
	}
	pool.QueryRow(ctx, `SELECT status FROM agent_outbox WHERE id=$1`, outboxID).Scan(&status)
	if status != "routed" {
		t.Errorf("status=%q, want routed", status)
	}

	// A second call should find nothing (no double-process).
	ok, err = ProcessOne(ctx, pool, "w1")
	if err != nil {
		t.Fatalf("ProcessOne 2: %v", err)
	}
	if ok {
		t.Error("second ProcessOne should find no work")
	}
}

// TestProcessOne_SelfObserverCollapsedByUnique covers the "no deliveries to
// self" requirement: when the agent's own origin topic is also bound as
// observer, the UNIQUE (outbox_id, channel, user_id, thread_id) collapses
// the duplicate row.
func TestProcessOne_SelfObserverCollapsedByUnique(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// alpha observes (u1, 100) — and also responds to an inbox row from
	// (u1, 100). Both paths target the same delivery key.
	mustExec(t, pool, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		VALUES (100, 'alpha', 'observer', 'u1', '100', -1001)
	`)

	inboxID := uuid.New()
	mustExec(t, pool, `
		INSERT INTO agent_inbox (id, agent_id, from_kind, origin_channel, origin_user_id, origin_thread_id, origin_chat_id, content)
		VALUES ($1, 'alpha', 'user', 'telegram', 'u1', '100', -1001, '{"text":"x"}'::jsonb)
	`, inboxID)

	outboxID := uuid.New()
	mustExec(t, pool, `
		INSERT INTO agent_outbox (id, agent_id, in_reply_to, content)
		VALUES ($1, 'alpha', $2, '{"text":"reply"}'::jsonb)
	`, outboxID, inboxID)

	if ok, err := ProcessOne(ctx, pool, "w1"); err != nil || !ok {
		t.Fatalf("ProcessOne: ok=%v err=%v", ok, err)
	}

	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM channel_deliveries WHERE outbox_id=$1`, outboxID).Scan(&count)
	if count != 1 {
		t.Errorf("delivery rows=%d, want 1 (origin+self-observer dedup)", count)
	}
}
