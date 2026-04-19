package mailbox

import "sync"

// ActiveInboxMap tracks the most-recently-claimed inbox row per agent.
// mailbox_consumer updates it before driving a message into the PTY;
// the monitor's outbox writer reads it to stamp in_reply_to on outbox
// rows so the relay can route responses to the origin Telegram topic.
type ActiveInboxMap struct {
	m sync.Map // key: agentID (string) → value: inbox row id (string)
}

// Set records that agentID is currently processing the given inbox row id.
func (a *ActiveInboxMap) Set(agentID, inboxID string) {
	a.m.Store(agentID, inboxID)
}

// Get returns the last inbox row id for agentID, or "" if none was set.
func (a *ActiveInboxMap) Get(agentID string) string {
	if v, ok := a.m.Load(agentID); ok {
		return v.(string)
	}
	return ""
}
