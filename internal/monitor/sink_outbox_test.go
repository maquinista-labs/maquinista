package monitor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// setupOutboxSink is a separate helper to avoid duplicate definition with setupOutbox.
// Both share the same DB structure; this one is used by OutboxSink tests.
func setupOutboxSink(t *testing.T) *OutboxSink {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('win-1','s','win-1')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return NewOutboxSink(pool, nil)
}

func TestOutboxSink_WritesAssistantText(t *testing.T) {
	s := setupOutboxSink(t)

	s.Handle(AgentEvent{
		Kind:    AgentEventText,
		AgentID: "win-1",
		Role:    "assistant",
		Text:    "hello world",
		ChatID:  0, // DB-only pass
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox WHERE agent_id='win-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("rows=%d, want 1", count)
	}

	var content []byte
	s.pool.QueryRow(context.Background(),
		`SELECT content FROM agent_outbox WHERE agent_id='win-1'`).Scan(&content)
	var decoded struct{ Text, Type, Role string }
	_ = json.Unmarshal(content, &decoded)
	if decoded.Type != "text" || decoded.Role != "assistant" {
		t.Errorf("content=%+v", decoded)
	}
	// render.FormatText returns the text unchanged
	if decoded.Text != "hello world" {
		t.Errorf("Text = %q, want hello world", decoded.Text)
	}
}

func TestOutboxSink_SkipsWhenChatIDNonZero(t *testing.T) {
	s := setupOutboxSink(t)

	s.Handle(AgentEvent{
		Kind:    AgentEventText,
		AgentID: "win-1",
		Role:    "assistant",
		Text:    "should be skipped",
		ChatID:  5, // non-zero → skip
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox`).Scan(&count)
	if count != 0 {
		t.Errorf("rows=%d, want 0", count)
	}
}

func TestOutboxSink_SkipsNonAssistant(t *testing.T) {
	s := setupOutboxSink(t)

	s.Handle(AgentEvent{
		Kind:    AgentEventText,
		AgentID: "win-1",
		Role:    "user", // not assistant
		Text:    "hi",
		ChatID:  0,
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox`).Scan(&count)
	if count != 0 {
		t.Errorf("rows=%d, want 0 for non-assistant", count)
	}
}

func TestOutboxSink_SkipsToolEvents(t *testing.T) {
	s := setupOutboxSink(t)

	s.Handle(AgentEvent{
		Kind:      AgentEventToolUse,
		AgentID:   "win-1",
		Role:      "assistant",
		Text:      "**bash**(ls)",
		ToolName:  "bash",
		ToolUseID: "toolu_01",
		ChatID:    0,
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox`).Scan(&count)
	if count != 0 {
		t.Errorf("rows=%d, want 0 for tool_use", count)
	}
}

func TestOutboxSink_WritesThinking(t *testing.T) {
	s := setupOutboxSink(t)

	s.Handle(AgentEvent{
		Kind:    AgentEventThinking,
		AgentID: "win-1",
		Role:    "assistant",
		Text:    "pondering...",
		ChatID:  0,
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox WHERE agent_id='win-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("rows=%d, want 1 for thinking", count)
	}
}

func TestOutboxSink_SkipsEmptyAgentID(t *testing.T) {
	s := setupOutboxSink(t)

	s.Handle(AgentEvent{
		Kind:    AgentEventText,
		AgentID: "", // empty
		Role:    "assistant",
		Text:    "hi",
		ChatID:  0,
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox`).Scan(&count)
	if count != 0 {
		t.Errorf("rows=%d, want 0 for empty AgentID", count)
	}
}

func TestOutboxSink_SkipsUnknownAgent(t *testing.T) {
	s := setupOutboxSink(t)

	// "ghost" has no row in agents table — resolveAgentFromWindow returns "".
	s.Handle(AgentEvent{
		Kind:    AgentEventText,
		AgentID: "ghost",
		Role:    "assistant",
		Text:    "hi",
		ChatID:  0,
	})

	var count int
	s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_outbox`).Scan(&count)
	if count != 0 {
		t.Errorf("rows=%d, want 0 for unknown agent", count)
	}
}
