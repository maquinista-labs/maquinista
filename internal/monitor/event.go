package monitor

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
}

func buildAgentEvent(windowID, agentID string, userID int64, threadID int, chatID int64, pe ParsedEntry) AgentEvent {
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
