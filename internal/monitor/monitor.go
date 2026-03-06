package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/otaviocarvalho/volta/internal/config"
	"github.com/otaviocarvalho/volta/internal/queue"
	"github.com/otaviocarvalho/volta/internal/render"
	"github.com/otaviocarvalho/volta/internal/state"
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

// Monitor polls Claude Code JSONL transcript files and routes entries to the message queue.
type Monitor struct {
	config             *config.Config
	state              *state.State
	monitorState       *state.MonitorState
	queue              *queue.Queue
	pendingTools       map[string]PendingTool
	fileMtimes         map[string]time.Time
	lastSessionMap     map[string]state.SessionMapEntry
	pollInterval       time.Duration
	turnStarts         sync.Map // windowID → time.Time
	PlanHandler        func(userID int64, threadID int, chatID int64, planJSON string)
	planBuffers        map[string]string // windowID → partial plan text
	ObservationLookup  ObservationLookup // optional: resolve window → observing topics
}

// New creates a new Monitor.
func New(cfg *config.Config, st *state.State, ms *state.MonitorState, q *queue.Queue) *Monitor {
	return &Monitor{
		config:         cfg,
		state:          st,
		monitorState:   ms,
		queue:          q,
		pendingTools:   make(map[string]PendingTool),
		fileMtimes:     make(map[string]time.Time),
		lastSessionMap: make(map[string]state.SessionMapEntry),
		pollInterval:   time.Duration(cfg.MonitorPollInterval * float64(time.Second)),
		planBuffers:    make(map[string]string),
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
			m.monitorState.ForceSave(filepath.Join(m.config.VoltaDir, "monitor_state.json"))
			log.Println("Session monitor stopped.")
			return
		case <-ticker.C:
			m.poll()
		}
	}
}

func (m *Monitor) poll() {
	// Load session_map.json
	sessionMapPath := filepath.Join(m.config.VoltaDir, "session_map.json")
	sm, err := state.LoadSessionMap(sessionMapPath)
	if err != nil {
		return
	}

	// Detect changes
	m.detectChanges(sm)

	// Process each active session
	for key, entry := range sm {
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}

		// Find the JSONL file for this session
		jsonlPath := m.findJSONLFile(entry.SessionID, entry.CWD)
		if jsonlPath == "" {
			continue
		}

		// Check mtime
		if !m.hasFileChanged(jsonlPath) {
			continue
		}

		// Read new content
		m.processSession(key, entry.SessionID, windowID, jsonlPath)
	}

	m.lastSessionMap = sm

	// Periodically save state
	monitorStatePath := filepath.Join(m.config.VoltaDir, "monitor_state.json")
	m.monitorState.SaveIfDirty(monitorStatePath)
}

func (m *Monitor) detectChanges(newMap map[string]state.SessionMapEntry) {
	// Clean up stale sessions
	for key := range m.lastSessionMap {
		if _, ok := newMap[key]; !ok {
			m.monitorState.RemoveSession(key)
			delete(m.fileMtimes, key)
		}
	}
}

func (m *Monitor) hasFileChanged(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	mtime := info.ModTime()
	lastMtime, ok := m.fileMtimes[path]
	if ok && mtime.Equal(lastMtime) {
		return false
	}

	m.fileMtimes[path] = mtime
	return true
}

func (m *Monitor) processSession(sessionKey, sessionID, windowID, jsonlPath string) {
	// Get current offset
	tracked, hasTracked := m.monitorState.GetTracked(sessionKey)
	var offset int64
	if hasTracked {
		offset = tracked.LastByteOffset
	}

	// Check file size (detect truncation from /clear)
	info, err := os.Stat(jsonlPath)
	if err != nil {
		return
	}
	if offset > info.Size() {
		offset = 0 // file was truncated
	}

	// Open and read new content
	f, err := os.Open(jsonlPath)
	if err != nil {
		return
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return
		}
	}

	var entries []*Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for large lines
	var bytesRead int64

	for scanner.Scan() {
		line := scanner.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for newline

		entry, err := ParseLine(line)
		if err != nil {
			log.Printf("JSONL parse error at offset %d: %v", offset+bytesRead, err)
			continue
		}
		if entry != nil {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("JSONL read error for %s at offset %d: %v (not advancing offset)", jsonlPath, offset+bytesRead, err)
		return // don't advance offset — will re-read on next poll
	}

	if len(entries) == 0 {
		// Update offset even if no entries (skip empty lines)
		if bytesRead > 0 {
			newOffset := offset + bytesRead
			m.monitorState.UpdateOffset(sessionKey, sessionID, jsonlPath, newOffset)
		}
		return
	}

	// Parse entries with tool pairing
	parsed := ParseEntries(entries, m.pendingTools)

	// Route to directly bound users (existing behavior)
	users := m.state.FindUsersForWindow(windowID)
	for _, ut := range users {
		chatID, ok := m.state.GetGroupChatID(ut.UserID, ut.ThreadID)
		if !ok {
			continue
		}
		threadID, _ := strconv.Atoi(ut.ThreadID)
		userID, _ := strconv.ParseInt(ut.UserID, 10, 64)

		for _, pe := range parsed {
			m.enqueueEntry(userID, threadID, chatID, windowID, pe)
		}
	}

	// Route to observing topics (agent observation model)
	if m.ObservationLookup != nil {
		observers := m.ObservationLookup(windowID)
		for _, obs := range observers {
			for _, pe := range parsed {
				m.enqueueEntry(obs.UserID, int(obs.TopicID), obs.ChatID, windowID, pe)
			}
		}
	}

	// Update offset
	newOffset := offset + bytesRead
	m.monitorState.UpdateOffset(sessionKey, sessionID, jsonlPath, newOffset)
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

// findJSONLFile locates the JSONL transcript file for a session.
func (m *Monitor) findJSONLFile(sessionID, cwd string) string {
	// First: check monitor state for cached path
	for _, key := range m.monitorState.AllKeys() {
		tracked, ok := m.monitorState.GetTracked(key)
		if ok && tracked.SessionID == sessionID && tracked.FilePath != "" {
			if _, err := os.Stat(tracked.FilePath); err == nil {
				return tracked.FilePath
			}
		}
	}

	// Second: scan ~/.claude/projects/ for matching session
	claudeDir := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		return ""
	}

	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}

		projectDir := filepath.Join(claudeDir, dir.Name())

		// Check sessions-index.json
		indexPath := filepath.Join(projectDir, "sessions-index.json")
		if path := m.searchSessionsIndex(indexPath, sessionID, projectDir); path != "" {
			return path
		}

		// Fallback: glob for JSONL files
		if path := m.searchJSONLFiles(projectDir, sessionID); path != "" {
			return path
		}
	}

	return ""
}

func (m *Monitor) searchSessionsIndex(indexPath, sessionID, projectDir string) string {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return ""
	}

	var index map[string]json.RawMessage
	if err := json.Unmarshal(data, &index); err != nil {
		return ""
	}

	for id := range index {
		if id == sessionID {
			jsonlPath := filepath.Join(projectDir, id+".jsonl")
			if _, err := os.Stat(jsonlPath); err == nil {
				return jsonlPath
			}
		}
	}
	return ""
}

func (m *Monitor) searchJSONLFiles(projectDir, sessionID string) string {
	matches, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	if err != nil {
		return ""
	}

	for _, match := range matches {
		base := filepath.Base(match)
		if strings.TrimSuffix(base, ".jsonl") == sessionID {
			return match
		}
	}
	return ""
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
