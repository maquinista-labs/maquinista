package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setupOutbox(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('win-1','s','win-1')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return pool
}

func TestDBOutboxWriter_WritesAssistantText(t *testing.T) {
	pool := setupOutbox(t)
	w := NewDBOutboxWriter(pool, nil)

	w(OutboxEvent{
		AgentID:  "win-1",
		UserID:   42,
		ThreadID: 100,
		ChatID:   -1001,
		Role:     "assistant",
		Text:     "hello world",
	})

	var count int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox WHERE agent_id='win-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("rows=%d, want 1", count)
	}

	var content []byte
	pool.QueryRow(context.Background(),
		`SELECT content FROM agent_outbox WHERE agent_id='win-1'`).Scan(&content)
	var decoded struct{ Text, Type, Role string }
	_ = json.Unmarshal(content, &decoded)
	if decoded.Text != "hello world" || decoded.Type != "text" || decoded.Role != "assistant" {
		t.Errorf("content=%+v", decoded)
	}
}

func TestDBOutboxWriter_ANSIAndHugeTextPreserved(t *testing.T) {
	pool := setupOutbox(t)
	w := NewDBOutboxWriter(pool, nil)

	ansi := "\x1b[31mred\x1b[0m \x1b[1mbold\x1b[0m"
	huge := strings.Repeat("line with some words — ", 4096) // ~96 KiB

	w(OutboxEvent{AgentID: "win-1", Role: "assistant", Text: ansi})
	w(OutboxEvent{AgentID: "win-1", Role: "assistant", Text: huge})

	rows, err := pool.Query(context.Background(),
		`SELECT content FROM agent_outbox WHERE agent_id='win-1' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var bodies []string
	for rows.Next() {
		var c []byte
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		var body struct{ Text string }
		_ = json.Unmarshal(c, &body)
		bodies = append(bodies, body.Text)
	}

	if len(bodies) != 2 {
		t.Fatalf("rows=%d, want 2", len(bodies))
	}
	if bodies[0] != ansi {
		t.Errorf("ANSI text mangled: got %q", bodies[0])
	}
	if bodies[1] != huge {
		t.Errorf("huge text truncated: len=%d, want %d", len(bodies[1]), len(huge))
	}
}

func TestDBOutboxWriter_SkipsEmptyAndMissingAgent(t *testing.T) {
	pool := setupOutbox(t)
	w := NewDBOutboxWriter(pool, nil)

	// empty text
	w(OutboxEvent{AgentID: "win-1", Role: "assistant", Text: ""})
	// empty agent id
	w(OutboxEvent{AgentID: "", Role: "assistant", Text: "x"})
	// unknown agent — the AppendOutbox FK rejects; the helper logs and moves on.
	w(OutboxEvent{AgentID: "ghost", Role: "assistant", Text: "x"})

	var count int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox`).Scan(&count)
	if count != 0 {
		t.Errorf("rows=%d, want 0", count)
	}
}

