package monitor

type OutputSink interface {
	Handle(e AgentEvent)
	Name() string
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
