package monitor

import "testing"

// stubSink records all events it receives.
type stubSink struct {
	name   string
	events []AgentEvent
}

func (s *stubSink) Handle(e AgentEvent) { s.events = append(s.events, e) }
func (s *stubSink) Name() string        { return s.name }

func TestMultiSink_FanOut(t *testing.T) {
	ms := NewMultiSink()
	s1 := &stubSink{name: "s1"}
	s2 := &stubSink{name: "s2"}
	s3 := &stubSink{name: "s3"}
	ms.Add(s1)
	ms.Add(s2)
	ms.Add(s3)

	ev := AgentEvent{Kind: AgentEventText, Text: "hello", WindowID: "w1"}
	ms.Emit(ev)

	for _, s := range []*stubSink{s1, s2, s3} {
		if len(s.events) != 1 {
			t.Errorf("sink %s: got %d events, want 1", s.name, len(s.events))
			continue
		}
		if s.events[0].Text != "hello" {
			t.Errorf("sink %s: Text = %q, want hello", s.name, s.events[0].Text)
		}
	}
}

func TestMultiSink_Empty(t *testing.T) {
	ms := NewMultiSink()
	// Should not panic with no sinks.
	ms.Emit(AgentEvent{Kind: AgentEventText, Text: "no panic"})
}

func TestMultiSink_Name(t *testing.T) {
	s := &stubSink{name: "my-sink"}
	var sink OutputSink = s
	if sink.Name() != "my-sink" {
		t.Errorf("Name() = %q, want my-sink", sink.Name())
	}
}

func TestMultiSink_MultipleEvents(t *testing.T) {
	ms := NewMultiSink()
	s := &stubSink{name: "s"}
	ms.Add(s)

	ms.Emit(AgentEvent{Kind: AgentEventText, Text: "first"})
	ms.Emit(AgentEvent{Kind: AgentEventThinking, Text: "second"})
	ms.Emit(AgentEvent{Kind: AgentEventToolUse, Text: "third"})

	if len(s.events) != 3 {
		t.Errorf("got %d events, want 3", len(s.events))
	}
}
