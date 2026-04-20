package monitor

import (
	"github.com/maquinista-labs/maquinista/internal/queue"
	"github.com/maquinista-labs/maquinista/internal/render"
)

type TelegramSink struct{ queue *queue.Queue }

func NewTelegramSink(q *queue.Queue) *TelegramSink { return &TelegramSink{queue: q} }

func (t *TelegramSink) Name() string { return "telegram" }

func (t *TelegramSink) Handle(e AgentEvent) {
	if e.ChatID == 0 || t.queue == nil {
		return
	}

	task := queue.MessageTask{
		UserID:   e.UserID,
		ThreadID: e.ThreadID,
		ChatID:   e.ChatID,
		WindowID: e.WindowID,
	}

	switch e.Kind {
	case AgentEventText:
		text := render.FormatText(e.Text)
		if e.Role == "user" {
			text = "\U0001F464 " + text
		}
		task.Parts = []string{text}
		task.ContentType = "content"
	case AgentEventThinking:
		task.Parts = []string{render.FormatThinking(e.Text)}
		task.ContentType = "content"
	case AgentEventToolUse:
		// Use the pre-formatted summary (e.Text) if available — ParseEntries
		// puts FormatToolUseSummary output there. Fall back to render.FormatToolUse
		// for callers that don't pre-format.
		text := e.Text
		if text == "" {
			text = render.FormatToolUse(e.ToolName, e.ToolInput)
		}
		task.Parts = []string{text}
		task.ContentType = "tool_use"
		task.ToolUseID = e.ToolUseID
	case AgentEventToolResult, AgentEventToolPaired:
		task.Parts = []string{render.FormatToolResult(e.ToolName, e.ToolInput, e.Text, e.IsError)}
		task.ContentType = "tool_result"
		task.ToolUseID = e.ToolUseID
	default:
		return
	}

	if len(task.Parts) == 0 || task.Parts[0] == "" {
		return
	}

	t.queue.Enqueue(task)
}
