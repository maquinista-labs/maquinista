package monitor

type OutputSink interface {
	Handle(e AgentEvent)
	Name() string
}

// SessionFlusher is an optional interface for sinks that buffer content
// within a session's poll cycle and need to be flushed at the end.
type SessionFlusher interface {
	FlushSession(windowID string)
}

type MultiSink struct {
	sinks []OutputSink
}

func NewMultiSink() *MultiSink { return &MultiSink{} }

func (ms *MultiSink) Add(s OutputSink) { ms.sinks = append(ms.sinks, s) }

func (ms *MultiSink) Emit(e AgentEvent) {
	for _, s := range ms.sinks {
		s.Handle(e)
	}
}

// FlushSession calls FlushSession on any sinks that implement SessionFlusher.
func (ms *MultiSink) FlushSession(windowID string) {
	for _, s := range ms.sinks {
		if f, ok := s.(SessionFlusher); ok {
			f.FlushSession(windowID)
		}
	}
}
