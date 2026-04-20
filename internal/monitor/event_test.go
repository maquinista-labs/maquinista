package monitor

import "testing"

func TestBuildAgentEvent_Text(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "text",
		Text:        "hello world",
	}
	e := buildAgentEvent("win1", "agent-1", 42, 100, -1001, pe)
	if e.Kind != AgentEventText {
		t.Errorf("Kind = %q, want AgentEventText", e.Kind)
	}
	if e.Role != "assistant" {
		t.Errorf("Role = %q, want assistant", e.Role)
	}
	if e.Text != "hello world" {
		t.Errorf("Text = %q, want hello world", e.Text)
	}
}

func TestBuildAgentEvent_TextUser(t *testing.T) {
	pe := ParsedEntry{
		Role:        "user",
		ContentType: "text",
		Text:        "hi there",
	}
	e := buildAgentEvent("win1", "agent-1", 7, 10, -200, pe)
	if e.Kind != AgentEventText {
		t.Errorf("Kind = %q, want AgentEventText", e.Kind)
	}
	if e.Role != "user" {
		t.Errorf("Role = %q, want user", e.Role)
	}
}

func TestBuildAgentEvent_Thinking(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "thinking",
		Text:        "I'm thinking...",
	}
	e := buildAgentEvent("win2", "agent-2", 1, 1, -999, pe)
	if e.Kind != AgentEventThinking {
		t.Errorf("Kind = %q, want AgentEventThinking", e.Kind)
	}
}

func TestBuildAgentEvent_ToolUse(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "tool_use",
		Text:        "**bash**(ls -la)",
		ToolName:    "bash",
		ToolUseID:   "toolu_01",
		// ToolInput is NOT set for tool_use (pre-formatted summary is in Text)
	}
	e := buildAgentEvent("win3", "agent-3", 5, 5, -500, pe)
	if e.Kind != AgentEventToolUse {
		t.Errorf("Kind = %q, want AgentEventToolUse", e.Kind)
	}
	if e.Text != "**bash**(ls -la)" {
		t.Errorf("Text = %q, want **bash**(ls -la)", e.Text)
	}
	if e.ToolInput != "" {
		t.Errorf("ToolInput = %q, want empty for tool_use", e.ToolInput)
	}
	if e.ToolUseID != "toolu_01" {
		t.Errorf("ToolUseID = %q, want toolu_01", e.ToolUseID)
	}
}

func TestBuildAgentEvent_ToolResultStandalone(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "tool_result",
		Text:        "some result",
		ToolName:    "unknown",
		ToolUseID:   "toolu_02",
	}
	e := buildAgentEvent("win4", "agent-4", 9, 9, -900, pe)
	if e.Kind != AgentEventToolResult {
		t.Errorf("Kind = %q, want AgentEventToolResult", e.Kind)
	}
}

func TestBuildAgentEvent_ToolResultStandaloneEmptyName(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "tool_result",
		Text:        "some result",
		ToolName:    "", // empty = standalone
		ToolUseID:   "toolu_03",
	}
	e := buildAgentEvent("win4", "agent-4", 9, 9, -900, pe)
	if e.Kind != AgentEventToolResult {
		t.Errorf("Kind = %q, want AgentEventToolResult for empty ToolName", e.Kind)
	}
}

func TestBuildAgentEvent_ToolResultPaired(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "tool_result",
		Text:        "command output",
		ToolName:    "bash",
		ToolInput:   "ls -la",
		ToolUseID:   "toolu_04",
	}
	e := buildAgentEvent("win5", "agent-5", 3, 3, -300, pe)
	if e.Kind != AgentEventToolPaired {
		t.Errorf("Kind = %q, want AgentEventToolPaired", e.Kind)
	}
	if e.ToolInput != "ls -la" {
		t.Errorf("ToolInput = %q, want ls -la", e.ToolInput)
	}
}

func TestBuildAgentEvent_PassthroughFields(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "text",
		Text:        "hi",
	}
	e := buildAgentEvent("my-window", "my-agent", 11, 22, -333, pe)
	if e.WindowID != "my-window" {
		t.Errorf("WindowID = %q, want my-window", e.WindowID)
	}
	if e.AgentID != "my-agent" {
		t.Errorf("AgentID = %q, want my-agent", e.AgentID)
	}
	if e.UserID != 11 {
		t.Errorf("UserID = %d, want 11", e.UserID)
	}
	if e.ThreadID != 22 {
		t.Errorf("ThreadID = %d, want 22", e.ThreadID)
	}
	if e.ChatID != -333 {
		t.Errorf("ChatID = %d, want -333", e.ChatID)
	}
}

func TestBuildAgentEvent_IsError(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "tool_result",
		Text:        "error text",
		ToolName:    "bash",
		ToolUseID:   "toolu_05",
		IsError:     true,
	}
	e := buildAgentEvent("w", "a", 1, 1, -1, pe)
	if !e.IsError {
		t.Error("IsError should be true")
	}
}

func TestBuildAgentEvent_UnknownContentType(t *testing.T) {
	pe := ParsedEntry{
		Role:        "assistant",
		ContentType: "unknown_type",
		Text:        "something",
	}
	e := buildAgentEvent("w", "a", 1, 1, -1, pe)
	// Kind should be zero value (empty string) for unrecognized types
	if e.Kind != "" {
		t.Errorf("Kind = %q, want empty for unknown content type", e.Kind)
	}
}
