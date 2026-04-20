package monitor

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/maquinista-labs/maquinista/internal/render"
)

type OutboxSink struct {
	pool    *pgxpool.Pool
	amap    *mailbox.ActiveInboxMap
	mu      sync.Mutex
	buffers map[string]string // windowID → accumulated text for current poll cycle
}

func NewOutboxSink(pool *pgxpool.Pool, amap *mailbox.ActiveInboxMap) *OutboxSink {
	return &OutboxSink{pool: pool, amap: amap, buffers: make(map[string]string)}
}

func (s *OutboxSink) Name() string { return "outbox" }

func formatToolUseForOutbox(toolName, toolInput string) string {
	if toolInput == "" {
		return "🖥 **" + toolName + "**"
	}
	// Trim long inputs to keep rows readable.
	preview := toolInput
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	return "🖥 **" + toolName + "**\n```\n" + preview + "\n```"
}

func formatToolResultForOutbox(toolName, text string, isError bool) string {
	prefix := "✓"
	if isError {
		prefix = "✗"
	}
	preview := text
	if len(preview) > 400 {
		preview = preview[:400] + "…"
	}
	if preview == "" {
		return prefix + " " + toolName
	}
	return prefix + " **" + toolName + "**\n```\n" + preview + "\n```"
}

func (s *OutboxSink) Handle(e AgentEvent) {
	// Only fire on the DB-only pass (chatID=0).
	if e.ChatID != 0 {
		return
	}
	if s.pool == nil || e.AgentID == "" {
		return
	}

	var text string
	switch e.Kind {
	case AgentEventText:
		if e.Role != "assistant" {
			return
		}
		text = render.FormatText(e.Text)
	case AgentEventThinking:
		if e.Role != "assistant" {
			return
		}
		text = render.FormatThinking(e.Text)
	case AgentEventToolPaired:
		// Only write when both tool_use and tool_result are available (paired).
		// Standalone AgentEventToolUse is skipped to avoid a duplicate: when the
		// paired event arrives in the next cycle, it would write the same tool call
		// again, producing two outbox bubbles for a single tool invocation.
		text = formatToolUseForOutbox(e.ToolName, e.ToolInput) +
			"\n\n" + formatToolResultForOutbox(e.ToolName, e.Text, e.IsError)
	default:
		return
	}
	if text == "" {
		return
	}

	// Buffer text per window — flush writes it as a single outbox row.
	// Buffer by WindowID — must match the key used in FlushSession.
	s.mu.Lock()
	existing := s.buffers[e.WindowID]
	if existing == "" {
		s.buffers[e.WindowID] = text
	} else {
		s.buffers[e.WindowID] = existing + "\n\n" + text
	}
	s.mu.Unlock()
	log.Printf("outbox sink: buffered kind=%s window=%s agent=%s len=%d", e.Kind, e.WindowID, e.AgentID, len(text))
}

// FlushSession writes all buffered text for the given window as a single
// outbox row, then clears the buffer. Called by MultiSink.FlushSession
// after each session's Pass-2 entries are processed.
func (s *OutboxSink) FlushSession(windowID string) {
	s.mu.Lock()
	text := s.buffers[windowID]
	delete(s.buffers, windowID)
	s.mu.Unlock()

	if strings.TrimSpace(text) == "" {
		return
	}

	ctx := context.Background()

	agentID, err := resolveAgentFromWindow(ctx, s.pool, windowID)
	if err != nil {
		log.Printf("outbox sink: resolve agent for window %s: %v", windowID, err)
		return
	}
	if agentID == "" {
		log.Printf("outbox sink: flush window=%s no agent found — skipping", windowID)
		return
	}
	log.Printf("outbox sink: flush window=%s agent=%s text_len=%d", windowID, agentID, len(text))

	content, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Role string `json:"role,omitempty"`
	}{Type: "text", Text: text, Role: "assistant"})
	if err != nil {
		log.Printf("outbox sink: marshal: %v", err)
		return
	}

	var inReplyTo *uuid.UUID
	if s.amap != nil {
		if raw := s.amap.Get(agentID); raw != "" {
			if id, parseErr := uuid.Parse(raw); parseErr == nil {
				inReplyTo = &id
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("outbox sink: begin: %v", err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := mailbox.AppendOutbox(ctx, tx, mailbox.OutboxMessage{
		AgentID:   agentID,
		InReplyTo: inReplyTo,
		Content:   content,
	}); err != nil {
		log.Printf("outbox sink: append: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("outbox sink: commit: %v", err)
		return
	}
	log.Printf("outbox sink: wrote row agent=%s text_len=%d", agentID, len(text))
}
