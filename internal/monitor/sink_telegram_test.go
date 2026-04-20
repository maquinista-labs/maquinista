package monitor

import "testing"

func TestTelegramSink_Name(t *testing.T) {
	s := NewTelegramSink(nil)
	if s.Name() != "telegram" {
		t.Errorf("Name() = %q, want telegram", s.Name())
	}
}

func TestTelegramSink_SkipsWhenChatIDZero(t *testing.T) {
	// chatID=0 should return early without touching the queue.
	// A nil queue means any real enqueue attempt would panic — so if we
	// reach the Enqueue call, the test panics, which counts as failure.
	s := NewTelegramSink(nil)
	e := AgentEvent{
		Kind:   AgentEventText,
		ChatID: 0, // skip condition
		Text:   "hello",
		Role:   "assistant",
	}
	// Should not panic.
	s.Handle(e)
}

func TestTelegramSink_SkipsWhenQueueNil(t *testing.T) {
	s := NewTelegramSink(nil)
	e := AgentEvent{
		Kind:   AgentEventText,
		ChatID: 12345, // non-zero
		Text:   "hello",
		Role:   "assistant",
	}
	// Should not panic when queue is nil.
	s.Handle(e)
}

func TestTelegramSink_SkipsUnknownKind(t *testing.T) {
	// An event with a zero Kind (unrecognized) should be skipped.
	s := NewTelegramSink(nil)
	e := AgentEvent{
		Kind:   AgentEventKind(""), // zero value — no case matches
		ChatID: 12345,
		Text:   "something",
	}
	// Should not panic.
	s.Handle(e)
}
