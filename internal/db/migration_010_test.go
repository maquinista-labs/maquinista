package db

import (
	"context"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func TestMigration010_AppliesCleanly(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	for _, tbl := range []string{"scheduled_jobs", "webhook_handlers"} {
		var ok bool
		pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)
		`, tbl).Scan(&ok)
		if !ok {
			t.Errorf("table %s missing", tbl)
		}
	}
}

func TestMigration010_FromKindAcceptsScheduledAndWebhook(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`)

	for _, kind := range []string{"scheduled", "webhook", "user", "agent", "system"} {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO agent_inbox (agent_id, from_kind, content)
			VALUES ('a', $1, '{"type":"text","text":"x"}'::jsonb)
		`, kind); err != nil {
			t.Errorf("from_kind=%s rejected: %v", kind, err)
		}
	}

	_, err := pool.Exec(context.Background(), `
		INSERT INTO agent_inbox (agent_id, from_kind, content)
		VALUES ('a', 'bogus', '{}'::jsonb)
	`)
	if err == nil {
		t.Error("from_kind='bogus' should be rejected")
	}
}

func TestMigration010_WebhookPathUniqueWhenEnabled(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	mustExec(t, pool, `INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('a','s','w')`)

	const tmpl = `/review-pr {{.number}}`
	mustExec(t, pool, `
		INSERT INTO webhook_handlers (name, path, secret, agent_id, prompt_template)
		VALUES ('h1', '/hooks/gh/pr', 's1', 'a', $1)
	`, tmpl)

	_, err := pool.Exec(context.Background(), `
		INSERT INTO webhook_handlers (name, path, secret, agent_id, prompt_template)
		VALUES ('h2', '/hooks/gh/pr', 's2', 'a', $1)
	`, tmpl)
	if err == nil {
		t.Fatal("expected unique-violation on second enabled handler for same path")
	}

	// A disabled duplicate is allowed (partial index).
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO webhook_handlers (name, path, secret, agent_id, prompt_template, enabled)
		VALUES ('h3', '/hooks/gh/pr', 's3', 'a', $1, FALSE)
	`, tmpl); err != nil {
		t.Errorf("disabled duplicate should be allowed: %v", err)
	}
}
