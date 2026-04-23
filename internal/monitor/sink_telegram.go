package monitor

// TelegramSink formerly forwarded agent events to Telegram via the legacy
// in-memory queue (internal/queue). That queue has been retired; Telegram
// delivery now goes through agent_outbox → channel_deliveries → dispatcher.
// The type is kept as an empty stub so any callers still compile while
// migration is in progress.

// TelegramSink is a retired sink — Handle is a no-op.
type TelegramSink struct{}

// NewTelegramSink returns a no-op TelegramSink. The queue argument is
// accepted for backwards compatibility but ignored.
func NewTelegramSink(_ interface{}) *TelegramSink { return &TelegramSink{} }

func (t *TelegramSink) Name() string        { return "telegram" }
func (t *TelegramSink) Handle(_ AgentEvent) {}
