package monitor

import (
	"context"
	"encoding/json"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ToolEventSink struct{ pool *pgxpool.Pool }

func NewToolEventSink(pool *pgxpool.Pool) *ToolEventSink { return &ToolEventSink{pool: pool} }

func (s *ToolEventSink) Name() string { return "tool_event" }

func (s *ToolEventSink) Handle(e AgentEvent) {
	// Only fire on the DB-only pass (chatID=0).
	if e.ChatID != 0 {
		return
	}
	if s.pool == nil || e.AgentID == "" || e.ToolUseID == "" {
		return
	}

	ctx := context.Background()

	// Resolve window ID to logical agent ID.
	agentID, err := resolveAgentFromWindow(ctx, s.pool, e.AgentID)
	if err != nil {
		log.Printf("tool_event sink: resolve agent for window %s: %v", e.AgentID, err)
		return
	}
	if agentID == "" {
		return
	}

	switch e.Kind {
	case AgentEventToolPaired:
		// Re-emit suppressed tool_use before tool_result so dashboard sees both.
		s.notify(ctx, agentID, "tool_use", e.ToolName, e.ToolUseID, e.ToolInput, "", false)
		s.notify(ctx, agentID, "tool_result", e.ToolName, e.ToolUseID, "", e.Text, e.IsError)
	case AgentEventToolUse:
		s.notify(ctx, agentID, "tool_use", e.ToolName, e.ToolUseID, e.ToolInput, "", false)
	case AgentEventToolResult:
		s.notify(ctx, agentID, "tool_result", e.ToolName, e.ToolUseID, "", e.Text, e.IsError)
	}
}

func (s *ToolEventSink) notify(ctx context.Context, agentID, typ, name, useID, input, text string, isError bool) {
	// Truncate long content to avoid bloated pg_notify payloads.
	if len(input) > 300 {
		input = input[:300] + "…"
	}
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	payload, _ := json.Marshal(map[string]any{
		"agent_id":    agentID,
		"type":        typ,
		"tool_name":   name,
		"tool_use_id": useID,
		"tool_input":  input,
		"text":        text,
		"is_error":    isError,
	})
	if _, err := s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", "tool_event", string(payload)); err != nil {
		log.Printf("tool_event sink: pg_notify: %v", err)
	}
}
