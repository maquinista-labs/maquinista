package monitor

import "time"

// staleThreshold is how old a JSONL entry can be (relative to when the monitor
// reads it) before it is considered stale. Stale events skip Telegram and
// dashboard tool_event routing to prevent flooding when the monitor catches up
// on a large backlog, while still being written to the outbox DB.
const staleThreshold = 15 * time.Second

type AgentEventKind string

const (
	AgentEventText       AgentEventKind = "text"
	AgentEventThinking   AgentEventKind = "thinking"
	AgentEventToolUse    AgentEventKind = "tool_use"
	AgentEventToolResult AgentEventKind = "tool_result"
	AgentEventToolPaired AgentEventKind = "tool_paired"
)

type AgentEvent struct {
	WindowID  string
	AgentID   string
	UserID    int64
	ThreadID  int
	ChatID    int64
	Kind      AgentEventKind
	Role      string
	Text      string
	ToolName  string
	ToolUseID string
	ToolInput string
	IsError   bool
	// IsStale is true when the JSONL entry is older than the staleness threshold,
	// meaning the monitor is catching up on backlog. Telegram and tool_event sinks
	// skip stale events to prevent flooding; OutboxSink always writes regardless.
	IsStale bool
}

func buildAgentEvent(windowID, agentID string, userID int64, threadID int, chatID int64, pe ParsedEntry) AgentEvent {
	isStale := !pe.Timestamp.IsZero() && time.Since(pe.Timestamp) > staleThreshold
	e := AgentEvent{
		WindowID:  windowID,
		AgentID:   agentID,
		UserID:    userID,
		ThreadID:  threadID,
		ChatID:    chatID,
		Role:      pe.Role,
		Text:      pe.Text,
		ToolName:  pe.ToolName,
		ToolUseID: pe.ToolUseID,
		ToolInput: pe.ToolInput,
		IsError:   pe.IsError,
		IsStale:   isStale,
	}
	switch pe.ContentType {
	case "text":
		e.Kind = AgentEventText
	case "thinking":
		e.Kind = AgentEventThinking
	case "tool_use":
		e.Kind = AgentEventToolUse
	case "tool_result":
		if pe.ToolName != "" && pe.ToolName != "unknown" {
			e.Kind = AgentEventToolPaired
		} else {
			e.Kind = AgentEventToolResult
		}
	}
	return e
}
