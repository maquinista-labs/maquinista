package monitor

import (
	"context"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/queue"
	"github.com/maquinista-labs/maquinista/internal/render"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// ObservationLookup resolves a tmux window to additional observing topics.
// Returns (topicID, chatID) pairs for topics observing the agent that owns this window.
// Implementations should look up the agent by window, then look up observing topics.
type ObservationLookup func(windowID string) []ObservingTopic

// OutboxEvent is one captured assistant response ready to be mirrored into
// agent_outbox. Used by OutboxWriter; keeps the monitor package free of
// direct DB access.
type OutboxEvent struct {
	AgentID   string // equal to WindowID by convention
	UserID    int64
	ThreadID  int
	ChatID    int64
	Role      string // 'assistant' | 'user' | 'thinking' | ...
	Text      string // rendered payload (the same string handed to the queue)
	InReplyTo string // optional: correlating inbox row id (currently unused — reserved for sidecar path)
}

// OutboxWriter mirrors every captured response into agent_outbox when set.
// A nil writer disables the mailbox.outbound path.
type OutboxWriter func(e OutboxEvent)

// ObservingTopic represents a topic that is observing an agent's output.
type ObservingTopic struct {
	TopicID int64
	ChatID  int64
	UserID  int64
}

// Monitor polls transcript sources and routes entries to the message queue.
type Monitor struct {
	config            *config.Config
	state             *state.State
	monitorState      *state.MonitorState
	queue             *queue.Queue
	sources           []TranscriptSource
	pollInterval      time.Duration
	turnStarts        sync.Map // windowID → time.Time
	PlanHandler       func(userID int64, threadID int, chatID int64, planJSON string)
	planBuffers       map[string]string // windowID → partial plan text
	ObservationLookup ObservationLookup // optional: resolve window → observing topics
	OutboxWriter      OutboxWriter      // optional: shadow-write captured responses to agent_outbox
	pollCount         int
}

// New creates a new Monitor.
func New(cfg *config.Config, st *state.State, ms *state.MonitorState, q *queue.Queue) *Monitor {
	return &Monitor{
		config:       cfg,
		state:        st,
		monitorState: ms,
		queue:        q,
		pollInterval: time.Duration(cfg.MonitorPollInterval * float64(time.Second)),
		planBuffers:  make(map[string]string),
	}
}

// AddSource adds a TranscriptSource to be polled by the monitor.
func (m *Monitor) AddSource(src TranscriptSource) {
	m.sources = append(m.sources, src)
}

// Run starts the monitor poll loop. Blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	log.Println("Session monitor starting...")
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.monitorState.ForceSave(filepath.Join(m.config.MaquinistaDir, "monitor_state.json"))
			log.Println("Session monitor stopped.")
			return
		case <-ticker.C:
			m.poll()
		}
	}
}

func (m *Monitor) poll() {
	m.pollCount++
	logSummary := m.pollCount%30 == 0 // ~1 min at 2s interval

	for _, src := range m.sources {
		sessions := src.DiscoverSessions()
		if logSummary {
			log.Printf("Monitor: source %s discovered %d sessions", src.Name(), len(sessions))
		}
		for _, sess := range sessions {
			// Check window is owned by this source
			if m.state.GetWindowRunner(sess.WindowID) != src.Name() {
				continue
			}

			// Get current offset
			tracked, hasTracked := m.monitorState.GetTracked(sess.Key)
			var offset int64
			if hasTracked {
				offset = tracked.LastByteOffset
			}

			// Read new entries from the source
			parsed, newOffset, err := src.ReadNewEntries(sess, offset)
			if err != nil {
				log.Printf("Monitor: error reading entries from %s session %s: %v", src.Name(), sess.Key, err)
				continue
			}

			// Update offset even if no parsed entries (source handles offset tracking internally)
			_ = newOffset

			if len(parsed) == 0 {
				continue
			}

			// Route to directly bound users.
			// Prefer the "active thread" — the single most recent (user, thread)
			// to route a message to this window — when available. This avoids
			// fanning out a reply to every topic that has ever bound to a shared
			// agent window via the §8.1 tier-3 ladder (cross-topic leak).
			var users []state.UserThread
			if ut, ok := m.state.GetActiveThread(sess.WindowID); ok {
				users = []state.UserThread{ut}
			} else {
				users = m.state.FindUsersForWindow(sess.WindowID)
			}
			for _, ut := range users {
				chatID, ok := m.state.GetGroupChatID(ut.UserID, ut.ThreadID)
				if !ok {
					continue
				}
				threadID, _ := strconv.Atoi(ut.ThreadID)
				userID, _ := strconv.ParseInt(ut.UserID, 10, 64)

				for _, pe := range parsed {
					m.enqueueEntry(userID, threadID, chatID, sess.WindowID, pe)
				}
			}

			// Route to observing topics (agent observation model)
			if m.ObservationLookup != nil {
				observers := m.ObservationLookup(sess.WindowID)
				for _, obs := range observers {
					for _, pe := range parsed {
						m.enqueueEntry(obs.UserID, int(obs.TopicID), obs.ChatID, sess.WindowID, pe)
					}
				}
			}

			// Write assistant responses to agent_outbox once per entry,
			// regardless of Telegram user binding. Dashboard-spawned agents
			// have no state binding (users == nil) so enqueueEntry is never
			// called for them — this ensures their outbox rows are written.
			if m.OutboxWriter != nil {
				for _, pe := range parsed {
					if pe.Role != "assistant" {
						continue
					}
					var text string
					switch pe.ContentType {
					case "text":
						text = render.FormatText(pe.Text)
					case "thinking":
						text = render.FormatThinking(pe.Text)
					}
					if text == "" {
						continue
					}
					m.OutboxWriter(OutboxEvent{
						AgentID: sess.WindowID,
						Role:    pe.Role,
						Text:    text,
					})
				}
			}
		}
	}

	// Periodically save state
	monitorStatePath := filepath.Join(m.config.MaquinistaDir, "monitor_state.json")
	m.monitorState.SaveIfDirty(monitorStatePath)
}

// SetTurnStart records the start time of a user turn for a window.
func (m *Monitor) SetTurnStart(windowID string) {
	m.turnStarts.Store(windowID, time.Now())
}

// GetAndClearTurnStart returns the turn start time and clears it.
func (m *Monitor) GetAndClearTurnStart(windowID string) (time.Time, bool) {
	v, ok := m.turnStarts.LoadAndDelete(windowID)
	if !ok {
		return time.Time{}, false
	}
	return v.(time.Time), true
}

func (m *Monitor) enqueueEntry(userID int64, threadID int, chatID int64, windowID string, pe ParsedEntry) {
	var text string
	var contentType string

	// Track turn start when we see a user entry
	if pe.Role == "user" && pe.ContentType == "text" {
		m.SetTurnStart(windowID)
	}

	// Detect PLAN_JSON: marker in assistant text
	if pe.Role == "assistant" && pe.ContentType == "text" && m.PlanHandler != nil {
		peText := pe.Text
		// Prepend any buffered partial plan from previous entry
		if buf, ok := m.planBuffers[windowID]; ok {
			peText = buf + peText
			delete(m.planBuffers, windowID)
		}
		if planJSON, rest, found := extractPlanJSON(peText); found {
			m.PlanHandler(userID, threadID, chatID, planJSON)
			if rest == "" {
				return
			}
			pe.Text = rest
		} else if strings.Contains(peText, "PLAN_JSON:") {
			// Marker found but JSON incomplete — buffer for next entry
			m.planBuffers[windowID] = peText
			return
		}
	}

	switch pe.ContentType {
	case "text":
		if pe.Role == "user" {
			text = "\U0001F464 " + render.FormatText(pe.Text)
		} else {
			text = render.FormatText(pe.Text)
		}
		contentType = "content"
	case "tool_use":
		text = render.FormatToolUse(pe.ToolName, "")
		if pe.Text != "" {
			text = pe.Text // use the pre-formatted summary
		}
		contentType = "tool_use"
	case "tool_result":
		text = render.FormatToolResult(pe.ToolName, pe.ToolInput, pe.Text, pe.IsError)
		contentType = "tool_result"
	case "thinking":
		text = render.FormatThinking(pe.Text)
		contentType = "content"
	default:
		return
	}

	if text == "" {
		return
	}

	m.queue.Enqueue(queue.MessageTask{
		UserID:      userID,
		ThreadID:    threadID,
		ChatID:      chatID,
		Parts:       []string{text},
		ContentType: contentType,
		ToolUseID:   pe.ToolUseID,
		WindowID:    windowID,
	})
}

// extractPlanJSON finds "PLAN_JSON:" marker followed by a JSON array,
// returns the JSON string, any remaining text after the array, and whether it was found.
func extractPlanJSON(text string) (jsonStr, rest string, found bool) {
	marker := "PLAN_JSON:"
	idx := strings.Index(text, marker)
	if idx < 0 {
		return "", "", false
	}

	after := text[idx+len(marker):]
	after = strings.TrimLeft(after, " \t\n\r")
	if len(after) == 0 || after[0] != '[' {
		return "", "", false
	}

	// Find matching closing bracket by depth
	depth := 0
	inString := false
	escaped := false
	for i, ch := range after {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '[' {
			depth++
		} else if ch == ']' {
			depth--
			if depth == 0 {
				jsonStr = after[:i+1]
				remaining := strings.TrimSpace(text[:idx] + after[i+1:])
				return jsonStr, remaining, true
			}
		}
	}

	// Unmatched brackets — incomplete JSON
	return "", "", false
}

// windowIDFromSessionKey extracts window ID from session key ("sessionName:@N" → "@N").
func windowIDFromSessionKey(key string) string {
	idx := strings.LastIndex(key, ":")
	if idx < 0 {
		return ""
	}
	return key[idx+1:]
}
