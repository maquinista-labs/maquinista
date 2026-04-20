package monitor

import (
	"context"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// ObservationLookup resolves a tmux window to additional observing topics.
// Returns (topicID, chatID) pairs for topics observing the agent that owns this window.
// Implementations should look up the agent by window, then look up observing topics.
type ObservationLookup func(windowID string) []ObservingTopic


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
	sink              *MultiSink
	pool              *pgxpool.Pool
	sources           []TranscriptSource
	pollInterval      time.Duration
	turnStarts        sync.Map // windowID → time.Time
	PlanHandler       func(userID int64, threadID int, chatID int64, planJSON string)
	planBuffers       map[string]string // windowID → partial plan text
	ObservationLookup ObservationLookup // optional: resolve window → observing topics
	pollCount         int
}

// New creates a new Monitor.
func New(cfg *config.Config, st *state.State, ms *state.MonitorState, sink *MultiSink, pool *pgxpool.Pool) *Monitor {
	return &Monitor{
		config:       cfg,
		state:        st,
		monitorState: ms,
		sink:         sink,
		pool:         pool,
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

			// Resolve agentID once per window per poll cycle.
			var agentID string
			if m.pool != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				agentID, _ = resolveAgentFromWindow(ctx, m.pool, sess.WindowID)
				cancel()
			}

			// Capture turn costs and strip usage entries before routing.
			parsed = m.captureAndStripUsage(parsed, agentID, sess.WindowID)

			// Prefer active thread; fall back to all bound users.
			var users []state.UserThread
			if ut, ok := m.state.GetActiveThread(sess.WindowID); ok {
				users = []state.UserThread{ut}
			} else {
				users = m.state.FindUsersForWindow(sess.WindowID)
			}

			// Pass 1: Telegram routing — one emit per (user/observer, entry).
			// OutboxSink and ToolEventSink skip these (chatID != 0).
			for _, ut := range users {
				chatID, ok := m.state.GetGroupChatID(ut.UserID, ut.ThreadID)
				if !ok {
					continue
				}
				threadID, _ := strconv.Atoi(ut.ThreadID)
				userID, _ := strconv.ParseInt(ut.UserID, 10, 64)

				for _, pe := range parsed {
					// Track turn start when we see a user entry
					if pe.Role == "user" && pe.ContentType == "text" {
						m.SetTurnStart(sess.WindowID)
					}

					// Plan detection (per-user; shared planBuffers means only fires once per window)
					if pe.Role == "assistant" && pe.ContentType == "text" && m.PlanHandler != nil {
						peText := pe.Text
						if buf, ok := m.planBuffers[sess.WindowID]; ok {
							peText = buf + peText
							delete(m.planBuffers, sess.WindowID)
						}
						if planJSON, rest, found := extractPlanJSON(peText); found {
							m.PlanHandler(userID, threadID, chatID, planJSON)
							if rest == "" {
								continue
							}
							pe.Text = rest
						} else if strings.Contains(peText, "PLAN_JSON:") {
							m.planBuffers[sess.WindowID] = peText
							continue
						}
					}

					if m.sink != nil {
						m.sink.Emit(buildAgentEvent(sess.WindowID, agentID, userID, threadID, chatID, pe))
					}
				}
			}

			// Observation topics (also Telegram-bound)
			if m.ObservationLookup != nil {
				observers := m.ObservationLookup(sess.WindowID)
				for _, obs := range observers {
					for _, pe := range parsed {
						if m.sink != nil {
							m.sink.Emit(buildAgentEvent(sess.WindowID, agentID, obs.UserID, int(obs.TopicID), obs.ChatID, pe))
						}
					}
				}
			}

			// Pass 2: DB-only — chatID=0. TelegramSink skips; OutboxSink and
			// ToolEventSink fire once per entry per session regardless of binding.
			if m.sink != nil {
				for _, pe := range parsed {
					m.sink.Emit(buildAgentEvent(sess.WindowID, agentID, 0, 0, 0, pe))
				}
				// Flush buffers (e.g. OutboxSink) after all entries for this session.
				m.sink.FlushSession(sess.WindowID)
			}
		}
	}

	monitorStatePath := filepath.Join(m.config.MaquinistaDir, "monitor_state.json")
	m.monitorState.SaveIfDirty(monitorStatePath)
}

// captureAndStripUsage processes usage-typed ParsedEntries, fires CaptureTurn
// for each, and returns the slice with usage entries removed so the rest of
// poll() only sees display-ready entries.
func (m *Monitor) captureAndStripUsage(entries []ParsedEntry, agentID, windowID string) []ParsedEntry {
	if m.pool == nil || agentID == "" {
		// Still strip usage entries even if we can't capture costs.
		filtered := entries[:0]
		for _, pe := range entries {
			if pe.ContentType != "usage" {
				filtered = append(filtered, pe)
			}
		}
		return filtered
	}

	startedAt, hasStart := m.GetAndClearTurnStart(windowID)

	filtered := entries[:0]
	for _, pe := range entries {
		if pe.ContentType != "usage" {
			filtered = append(filtered, pe)
			continue
		}
		if pe.Usage == nil {
			continue
		}
		u := pe.Usage
		tc := TurnCost{
			AgentID:      agentID,
			Model:        u.Model,
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			CacheRead:    u.CacheRead,
			CacheWrite:   u.CacheWrite,
			FinishedAt:   u.Timestamp,
		}
		if hasStart {
			tc.StartedAt = startedAt
		}
		go func(t TurnCost) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := CaptureTurn(ctx, m.pool, t); err != nil {
				log.Printf("monitor: CaptureTurn agent=%s model=%s: %v", t.AgentID, t.Model, err)
			}
		}(tc)
	}
	return filtered
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
