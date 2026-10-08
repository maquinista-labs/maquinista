package monitor

import (
	"context"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// transcriptTouchInterval throttles the transcript-liveness writes: a
// streaming agent advances its transcript on every poll (~2s), while the
// pipeline watchdog's stall bound is hours — minute granularity is plenty.
const transcriptTouchInterval = 30 * time.Second

// Monitor polls transcript sources and routes entries to the message queue.
type Monitor struct {
	config              *config.Config
	state               *state.State
	monitorState        *state.MonitorState
	sink                *MultiSink
	pool                *pgxpool.Pool
	sources             []TranscriptSource
	pollInterval        time.Duration
	turnStarts          sync.Map // windowID → time.Time
	PlanHandler         func(userID int64, threadID int, chatID int64, planJSON string)
	planBuffers         map[string]string // windowID → partial plan text
	ObservationLookup   ObservationLookup // optional: resolve window → observing topics
	pollCount           int
	transcriptTouches   sync.Map      // windowID → time.Time of last liveness write
	transcriptTouchFreq time.Duration // liveness write throttle (default transcriptTouchInterval; 0 disables)

	// Turn-end shadow signal (ADR-0008 F1, MAQ-43): detection on transcript
	// growth, emission = journal only. turnEnds dedups per window on the
	// closing message id; turnEndCount is the boot-cumulative incidence.
	turnEnds     sync.Map // windowID → last fired closing message id
	turnEndCount atomic.Uint64
}

// New creates a new Monitor.
func New(cfg *config.Config, st *state.State, ms *state.MonitorState, sink *MultiSink, pool *pgxpool.Pool) *Monitor {
	return &Monitor{
		config:              cfg,
		state:               st,
		monitorState:        ms,
		sink:                sink,
		pool:                pool,
		pollInterval:        time.Duration(cfg.MonitorPollInterval * float64(time.Second)),
		planBuffers:         make(map[string]string),
		transcriptTouchFreq: transcriptTouchInterval,
	}
}

// touchTranscriptLiveness records transcript growth for the agent owning
// windowID (MAQ-9): agents.last_transcript_at = NOW(). The pipeline
// watchdog treats any growth inside the stall window as liveness — a
// healthy agent mid-command (go test, build) streams tool events into the
// transcript but writes zero agent_outbox rows, so outbox silence alone
// must not read as stalled. Throttled to one write per
// transcriptTouchFreq per window; best-effort (failures are logged, never
// fail the poll pass). Windows that resolve to no agent row (user
// sessions, dead rows) are silently skipped.
func (m *Monitor) touchTranscriptLiveness(windowID string) {
	if m.pool == nil {
		return
	}
	if v, ok := m.transcriptTouches.Load(windowID); ok {
		if time.Since(v.(time.Time)) < m.transcriptTouchFreq {
			return
		}
	}
	// Store before writing: the throttle bounds the query rate even when
	// the write loses the race or the DB hiccups — the next growth tick
	// retries after one interval, far inside any stall window.
	m.transcriptTouches.Store(windowID, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agentID, err := resolveAgentFromWindow(ctx, m.pool, windowID)
	if err != nil || agentID == "" {
		return
	}
	if _, err := m.pool.Exec(ctx,
		`UPDATE agents SET last_transcript_at = NOW() WHERE id = $1`, agentID); err != nil {
		log.Printf("monitor: transcript liveness update agent=%s: %v", agentID, err)
	}
}

// AddSource adds a TranscriptSource to be polled by the monitor.
func (m *Monitor) AddSource(src TranscriptSource) {
	m.sources = append(m.sources, src)
}

// recordTurnEnd is the ADR-0008 turn-end producer: when a poll batch ends
// with an assistant text message, the transcript tail shows an assistant
// message closing the turn (mid-turn batches end on tool calls / tool
// results; pi's final assistant message is the turn's last entry). Sticky
// evidence on agents.last_turn_end_at for the pipeline's nudge legs and
// the freeze-cause classifier — never cleared by consumers. Best-effort,
// mirrors touchTranscriptLiveness: failures log, never fail the poll;
// windows that resolve to no agent row are silently skipped.
func (m *Monitor) recordTurnEnd(agentID string) {
	if m.pool == nil || agentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := m.pool.Exec(ctx,
		`UPDATE agents SET last_turn_end_at = NOW() WHERE id = $1`, agentID); err != nil {
		log.Printf("monitor: turn-end update agent=%s: %v", agentID, err)
	}
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

			// Sources self-track their read offsets internally; the returned
			// offset is the transcript-growth signal (MAQ-9): any advance is
			// liveness, even with zero displayable entries (tool events
			// mid-command, metadata lines).
			if newOffset > offset {
				m.touchTranscriptLiveness(sess.WindowID)
				// Growth is also the trigger to re-inspect the transcript
				// tail for a turn close (ADR-0008 F1, shadow: log + count
				// only). Unchanged tails cannot produce a new closing.
				m.detectTurnEnd(src, sess)
			}

			if len(parsed) == 0 {
				continue
			}

			log.Printf("monitor: window=%s source=%s new_entries=%d", sess.WindowID, src.Name(), len(parsed))

			// Resolve agentID once per window per poll cycle.
			var agentID string
			if m.pool != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				agentID, _ = resolveAgentFromWindow(ctx, m.pool, sess.WindowID)
				cancel()
			}
			log.Printf("monitor: window=%s resolved_agent=%q", sess.WindowID, agentID)

			// Capture turn costs and strip usage entries before routing.
			parsed = m.captureAndStripUsage(parsed, agentID, sess.WindowID)

			// ADR-0008: a batch ending on assistant text is a turn end —
			// record the sticky signal (pipeline nudge legs + freeze-cause
			// classifier consume it).
			if batchEndsWithTurnEnd(parsed) {
				m.recordTurnEnd(agentID)
			}

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
				log.Printf("monitor: pass2 window=%s entries=%d", sess.WindowID, len(parsed))
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
		model := u.Model
		if model == "<synthetic>" {
			model = "orchestration"
		}
		tc := TurnCost{
			AgentID:      agentID,
			Model:        model,
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

// batchEndsWithTurnEnd reports whether a parsed batch closes a turn
// (ADR-0008): the transcript tail shows an assistant message with no
// pending tool call after it. Mid-turn batches end on tool_use,
// tool_result, or thinking — only a trailing assistant text entry closes a
// turn. pi's final assistant message is the turn's last entry; the
// claude/opencode sources' ParseEntries shape matches (assistant text
// trails, tool calls pair before it).
func batchEndsWithTurnEnd(parsed []ParsedEntry) bool {
	if len(parsed) == 0 {
		return false
	}
	last := parsed[len(parsed)-1]
	return last.Role == "assistant" && last.ContentType == "text"
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
