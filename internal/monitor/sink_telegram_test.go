package monitor

import "testing"

func TestTelegramSink_Name(t *testing.T) {
	s := NewTelegramSink(nil)
	if s.Name() != "telegram" {
		t.Errorf("Name() = %q, want telegram", s.Name())
	}
}

func TestTelegramSink_HandleIsNoop(t *testing.T) {
	s := NewTelegramSink(nil)
	// Handle must be a no-op and must not panic regardless of input.
	s.Handle(AgentEvent{Kind: AgentEventText, ChatID: 12345, Text: "hello"})
}
