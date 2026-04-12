package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
	"github.com/maquinista-labs/maquinista/internal/state"
)

func TestMigration009_AppliesCleanly(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)

	applied, err := RunMigrations(pool)
	if err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	has := func(name string) bool {
		for _, n := range applied {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("009_mailbox.sql") {
		t.Fatalf("009_mailbox.sql was not applied; got %v", applied)
	}

	for _, tbl := range []string{
		"topic_agent_bindings", "agent_topic_sessions", "conversations",
		"agent_inbox", "agent_outbox", "channel_deliveries",
		"message_attachments", "agent_settings",
	} {
		var exists bool
		err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema='public' AND table_name=$1
			)
		`, tbl).Scan(&exists)
		if err != nil || !exists {
			t.Errorf("table %s not present (err=%v)", tbl, err)
		}
	}

	var col string
	err = pool.QueryRow(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_name='agents' AND column_name='stop_requested'
	`).Scan(&col)
	if err != nil {
		t.Errorf("agents.stop_requested missing: %v", err)
	}

	err = pool.QueryRow(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_name='agent_settings' AND column_name='is_default'
	`).Scan(&col)
	if err != nil {
		t.Errorf("agent_settings.is_default missing: %v", err)
	}
}

func TestMigration009_OwnerBindingUnique(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a1','s','w1'), ('a2','s','w2')`)

	if _, err := pool.Exec(ctx, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id)
		VALUES (100, 'a1', 'owner', 'u1', '100')
	`); err != nil {
		t.Fatalf("insert first owner: %v", err)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id)
		VALUES (100, 'a2', 'owner', 'u1', '100')
	`)
	if err == nil {
		t.Fatal("expected unique-violation on second owner for (u1, 100)")
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id)
		VALUES (100, 'a2', 'observer', 'u1', '100')
	`); err != nil {
		t.Fatalf("observer insert should succeed: %v", err)
	}
}

func TestMigration009_NotifyAgentInbox(t *testing.T) {
	pool, dsn := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('agentA','s','w')`)

	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("listener connect: %v", err)
	}
	defer listener.Close(ctx)

	if _, err := listener.Exec(ctx, "LISTEN agent_inbox_new"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_inbox (agent_id, from_kind, content)
		VALUES ('agentA', 'user', '{"type":"text","text":"hi"}'::jsonb)
	`); err != nil {
		t.Fatalf("insert inbox: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := listener.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if n.Channel != "agent_inbox_new" {
		t.Errorf("channel = %q", n.Channel)
	}
	if n.Payload != "agentA" {
		t.Errorf("payload = %q, want agentA", n.Payload)
	}
}

func TestMigration009_BackfillIdempotent(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	mustExec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window)
		VALUES ('win-1','s','win-1'), ('win-2','s','win-2')
	`)

	st := state.NewState()
	st.BindThread("user-1", "100", "win-1")
	st.BindThread("user-1", "200", "win-2")
	st.BindThread("user-2", "300", "win-missing")
	st.SetGroupChatID("user-1", "100", -1001)
	st.SetGroupChatID("user-1", "200", -1002)

	ctx := context.Background()
	ins1, skip1, err := BackfillThreadBindings(ctx, pool, st)
	if err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	if ins1 != 2 {
		t.Errorf("first run inserted %d, want 2", ins1)
	}
	if skip1 != 1 {
		t.Errorf("first run skipped %d, want 1 (win-missing)", skip1)
	}

	ins2, _, err := BackfillThreadBindings(ctx, pool, st)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if ins2 != 0 {
		t.Errorf("second run inserted %d, want 0 (idempotent)", ins2)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM topic_agent_bindings WHERE binding_type='owner'
	`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("owner rows = %d, want 2", count)
	}

	var chatID int64
	if err := pool.QueryRow(ctx, `
		SELECT chat_id FROM topic_agent_bindings
		WHERE user_id='user-1' AND thread_id='100' AND binding_type='owner'
	`).Scan(&chatID); err != nil {
		t.Fatalf("chat_id lookup: %v", err)
	}
	if chatID != -1001 {
		t.Errorf("chat_id = %d, want -1001", chatID)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", fmt.Sprintf("%.80s", sql), err)
	}
}
