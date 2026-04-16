package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
	_ "modernc.org/sqlite"
)

// OpenCodeSource implements TranscriptSource for OpenCode. Session
// discovery pulls tmux + cwd + window-created-at from Postgres agents
// (Phase A of plans/active/json-state-migration.md), then cross-references
// OpenCode's own SQLite DB to find the runner session id — which we write
// back to agents.session_id so the bot can attribute subsequent responses.
type OpenCodeSource struct {
	config         *config.Config
	pool           *pgxpool.Pool
	appState       *state.State
	monitorState   *state.MonitorState
	dbPath         string
	db             *sql.DB
	knownTools     map[string]string // part ID → last emitted status (dedup)
	lastSessionMap map[string]state.SessionMapEntry
}

// NewOpenCodeSource creates a new OpenCodeSource.
func NewOpenCodeSource(cfg *config.Config, pool *pgxpool.Pool, st *state.State, ms *state.MonitorState) *OpenCodeSource {
	return &OpenCodeSource{
		config:         cfg,
		pool:           pool,
		appState:       st,
		monitorState:   ms,
		dbPath:         openCodeDBPath(),
		knownTools:     make(map[string]string),
		lastSessionMap: make(map[string]state.SessionMapEntry),
	}
}

func (o *OpenCodeSource) Name() string {
	return "opencode"
}

func openCodeDBPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

func (o *OpenCodeSource) ensureDB() error {
	if o.db != nil {
		return nil
	}
	if _, err := os.Stat(o.dbPath); err != nil {
		log.Printf("OpenCode DB not found at %s: %v", o.dbPath, err)
		return fmt.Errorf("opencode db not found: %w", err)
	}
	db, err := sql.Open("sqlite", o.dbPath+"?mode=ro&_journal_mode=wal")
	if err != nil {
		log.Printf("OpenCode DB open failed (%s): %v", o.dbPath, err)
		return fmt.Errorf("opening opencode db: %w", err)
	}
	// Verify the connection actually works — sql.Open may succeed lazily.
	if err := db.Ping(); err != nil {
		db.Close()
		log.Printf("OpenCode DB ping failed (%s): %v", o.dbPath, err)
		return fmt.Errorf("pinging opencode db: %w", err)
	}
	o.db = db
	log.Printf("OpenCode DB opened: %s", o.dbPath)
	return nil
}

// resetDB closes and clears the cached DB handle so the next ensureDB retries.
func (o *OpenCodeSource) resetDB() {
	if o.db != nil {
		o.db.Close()
		o.db = nil
	}
}

func (o *OpenCodeSource) DiscoverSessions() []ActiveSession {
	if o.pool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sm, windowCreatedAt, err := loadOpenCodeSessionMap(ctx, o.pool)
	if err != nil {
		log.Printf("opencode source: session discovery: %v", err)
		return nil
	}

	// Clean up stale sessions.
	for key := range o.lastSessionMap {
		if _, ok := sm[key]; !ok {
			o.monitorState.RemoveSession(key)
		}
	}

	// Discover or re-discover session IDs. OpenCode may create new sessions
	// for the same directory (e.g. on restart), so we always check the
	// latest session and update if it changed.
	for key, entry := range sm {
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}
		if o.appState.GetWindowRunner(windowID) != "opencode" {
			continue
		}

		discovered, derr := o.discoverSession(entry.CWD, windowCreatedAt[key])
		if derr != nil {
			log.Printf("OpenCode: failed to discover session for %s (window %s): %v", entry.CWD, windowID, derr)
			continue
		}
		if discovered == entry.SessionID {
			continue
		}
		old := entry.SessionID
		entry.SessionID = discovered
		sm[key] = entry

		// Persist discovered session id to agents.session_id so the bot and
		// follow-up polls see it without rediscovering.
		if _, uerr := o.pool.Exec(ctx, `
			UPDATE agents SET session_id=$1, last_seen=NOW()
			WHERE tmux_session=$2 AND tmux_window=$3
		`, discovered, strings.SplitN(key, ":", 2)[0], strings.SplitN(key, ":", 2)[1]); uerr != nil {
			log.Printf("OpenCode: persist session_id: %v", uerr)
		}

		if old == "" {
			currentTime := o.getCurrentTimeUpdated(discovered)
			if currentTime > 0 {
				o.monitorState.UpdateOffset(key, discovered, "opencode:sqlite", currentTime)
			} else {
				o.monitorState.RemoveSession(key)
			}
			log.Printf("OpenCode session discovered: %s -> %s (offset: %d)", entry.CWD, discovered, currentTime)
		} else {
			o.monitorState.RemoveSession(key)
			log.Printf("OpenCode session changed: %s -> %s (was %s)", entry.CWD, discovered, old)
		}
	}

	var sessions []ActiveSession
	for key, entry := range sm {
		if entry.SessionID == "" {
			continue
		}
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}
		if o.appState.GetWindowRunner(windowID) != "opencode" {
			continue
		}
		sessions = append(sessions, ActiveSession{
			Key:      key,
			WindowID: windowID,
		})
	}

	o.lastSessionMap = sm
	return sessions
}

// loadOpenCodeSessionMap returns (sessionMap, windowCreatedAt-in-unix-ms)
// for every live opencode agent. The started_at of the agents row stands
// in for "window creation time" so OpenCode's time-scoped session-discovery
// query can isolate sessions to a specific window spawn.
func loadOpenCodeSessionMap(ctx context.Context, pool *pgxpool.Pool) (map[string]state.SessionMapEntry, map[string]int64, error) {
	rows, err := pool.Query(ctx, `
		SELECT tmux_session, tmux_window,
		       COALESCE(session_id,''), COALESCE(cwd,''), COALESCE(window_name,''),
		       COALESCE(EXTRACT(EPOCH FROM started_at)*1000, 0)::bigint
		FROM agents
		WHERE runner_type = 'opencode'
		  AND status IN ('running','working','idle')
		  AND tmux_session <> '' AND tmux_window <> ''
	`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	sm := map[string]state.SessionMapEntry{}
	createdAt := map[string]int64{}
	for rows.Next() {
		var tmuxSession, tmuxWindow, sessionID, cwd, windowName string
		var startedAtMS int64
		if err := rows.Scan(&tmuxSession, &tmuxWindow, &sessionID, &cwd, &windowName, &startedAtMS); err != nil {
			return nil, nil, err
		}
		key := tmuxSession + ":" + tmuxWindow
		sm[key] = state.SessionMapEntry{
			SessionID:  sessionID,
			CWD:        cwd,
			WindowName: windowName,
		}
		createdAt[key] = startedAtMS
	}
	return sm, createdAt, rows.Err()
}

// discoverSession finds the OpenCode session for a given directory.
// If windowCreatedAt > 0 (Unix ms), it looks for the session created at or after that
// timestamp, which isolates the session to the specific window that was spawned.
// Falls back to the latest session if no time-scoped match is found.
func (o *OpenCodeSource) discoverSession(directory string, windowCreatedAt int64) (string, error) {
	if err := o.ensureDB(); err != nil {
		return "", err
	}
	var sessionID string

	if windowCreatedAt > 0 {
		err := o.db.QueryRow(
			`SELECT id FROM session WHERE directory = ? AND time_created >= ? ORDER BY time_created ASC LIMIT 1`,
			directory, windowCreatedAt,
		).Scan(&sessionID)
		if err == nil {
			return sessionID, nil
		}
		// No session found yet (OpenCode still starting) or timestamp unit mismatch — fall through.
	}

	err := o.db.QueryRow(
		`SELECT id FROM session WHERE directory = ? ORDER BY time_created DESC LIMIT 1`,
		directory,
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("discovering session for %s: %w", directory, err)
	}
	return sessionID, nil
}

// getCurrentTimeUpdated returns the latest time_updated for a session,
// used to initialize new windows to avoid replaying history.
func (o *OpenCodeSource) getCurrentTimeUpdated(sessionID string) int64 {
	if err := o.ensureDB(); err != nil {
		return 0
	}
	var maxTime int64
	err := o.db.QueryRow(
		`SELECT COALESCE(MAX(time_updated), 0) FROM part WHERE session_id = ?`,
		sessionID,
	).Scan(&maxTime)
	if err != nil {
		return 0
	}
	return maxTime
}

func (o *OpenCodeSource) ReadNewEntries(session ActiveSession, lastOffset int64) ([]ParsedEntry, int64, error) {
	if err := o.ensureDB(); err != nil {
		log.Printf("OpenCode: DB unavailable for session %s: %v", session.Key, err)
		return nil, lastOffset, nil
	}

	entry, ok := o.lastSessionMap[session.Key]
	if !ok || entry.SessionID == "" {
		return nil, lastOffset, nil
	}

	// lastOffset stores the last time_updated value for OpenCode
	lastTime := lastOffset

	rows, err := o.db.Query(`
		SELECT p.id, p.data, p.time_updated, m.data AS msg_data
		FROM part p
		JOIN message m ON p.message_id = m.id
		WHERE p.session_id = ? AND p.time_updated > ?
		ORDER BY p.time_created ASC
	`, entry.SessionID, lastTime)
	if err != nil {
		log.Printf("OpenCode poll error for session %s: %v", entry.SessionID, err)
		o.resetDB() // connection may be stale; retry on next poll
		return nil, lastOffset, nil
	}
	defer rows.Close()

	var entries []ParsedEntry
	var maxTime int64

	for rows.Next() {
		var partID string
		var partDataRaw, msgDataRaw string
		var timeUpdated int64

		if err := rows.Scan(&partID, &partDataRaw, &timeUpdated, &msgDataRaw); err != nil {
			log.Printf("OpenCode scan error: %v", err)
			continue
		}

		if timeUpdated > maxTime {
			maxTime = timeUpdated
		}

		var msgData openCodeMessage
		if err := json.Unmarshal([]byte(msgDataRaw), &msgData); err != nil {
			continue
		}

		var partData openCodePartData
		if err := json.Unmarshal([]byte(partDataRaw), &partData); err != nil {
			continue
		}

		pe := o.partToParsedEntry(partID, partData, msgData.Role)
		if pe != nil {
			entries = append(entries, *pe)
		}
	}

	newOffset := lastOffset
	if maxTime > 0 {
		newOffset = maxTime
		o.monitorState.UpdateOffset(session.Key, entry.SessionID, "opencode:sqlite", maxTime)
	}

	return entries, newOffset, nil
}

func (o *OpenCodeSource) ExtractStatusLine(paneText string) (string, bool) {
	// OpenCode shows status in the bottom bar; detect spinning/working indicators
	lines := strings.Split(paneText, "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-3; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.Contains(line, "Build") && strings.Contains(line, "s") {
			return line, true
		}
	}
	return "", false
}

func (o *OpenCodeSource) IsInteractiveUI(paneText string) bool {
	// OpenCode doesn't have permission prompts like Claude Code
	return false
}

// openCodeMessage is the JSON structure in message.data.
type openCodeMessage struct {
	Role string `json:"role"`
}

// openCodePartData is the JSON structure in part.data.
type openCodePartData struct {
	Type      string             `json:"type"`
	Text      string             `json:"text,omitempty"`
	Reasoning string             `json:"reasoning,omitempty"`
	State     *openCodeToolState `json:"state,omitempty"`
	Tool      string             `json:"tool,omitempty"`
}

type openCodeToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

func (o *OpenCodeSource) partToParsedEntry(partID string, part openCodePartData, role string) *ParsedEntry {
	switch part.Type {
	case "text":
		if part.Text == "" {
			return nil
		}
		return &ParsedEntry{
			Role:        role,
			ContentType: "text",
			Text:        part.Text,
		}

	case "reasoning":
		text := part.Reasoning
		if text == "" {
			text = part.Text
		}
		if text == "" {
			return nil
		}
		return &ParsedEntry{
			Role:        "assistant",
			ContentType: "thinking",
			Text:        text,
		}

	case "tool":
		if part.State == nil {
			return nil
		}
		return o.handleToolPart(partID, part)

	default:
		return nil
	}
}

func (o *OpenCodeSource) handleToolPart(partID string, part openCodePartData) *ParsedEntry {
	status := part.State.Status
	lastStatus := o.knownTools[partID]

	switch status {
	case "running":
		if lastStatus == "running" {
			return nil
		}
		o.knownTools[partID] = "running"

		inputStr := ExtractToolInput(part.Tool, part.State.Input)

		return &ParsedEntry{
			ContentType: "tool_use",
			ToolName:    part.Tool,
			ToolInput:   inputStr,
			ToolUseID:   partID,
			Text:        FormatToolUseSummary(part.Tool, inputStr),
		}

	case "completed":
		if lastStatus == "completed" {
			return nil
		}
		o.knownTools[partID] = "completed"

		inputStr := ExtractToolInput(part.Tool, part.State.Input)

		return &ParsedEntry{
			ContentType: "tool_result",
			ToolName:    part.Tool,
			ToolInput:   inputStr,
			ToolUseID:   partID,
			Text:        part.State.Output,
		}

	case "error":
		if lastStatus == "error" {
			return nil
		}
		o.knownTools[partID] = "error"

		inputStr := ExtractToolInput(part.Tool, part.State.Input)

		return &ParsedEntry{
			ContentType: "tool_result",
			ToolName:    part.Tool,
			ToolInput:   inputStr,
			ToolUseID:   partID,
			Text:        part.State.Output,
			IsError:     true,
		}

	default:
		return nil
	}
}

// Close closes the underlying database connection.
func (o *OpenCodeSource) Close() {
	if o.db != nil {
		o.db.Close()
		o.db = nil
	}
}
