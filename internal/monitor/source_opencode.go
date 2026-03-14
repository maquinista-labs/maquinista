package monitor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/otaviocarvalho/volta/internal/config"
	"github.com/otaviocarvalho/volta/internal/state"
	_ "modernc.org/sqlite"
)

// OpenCodeSource implements TranscriptSource for OpenCode.
// It discovers sessions via session_map.json and reads transcript parts from OpenCode's SQLite database.
type OpenCodeSource struct {
	config         *config.Config
	monitorState   *state.MonitorState
	dbPath         string
	db             *sql.DB
	knownTools     map[string]string // part ID → last emitted status (dedup)
	lastSessionMap map[string]state.SessionMapEntry
}

// NewOpenCodeSource creates a new OpenCodeSource.
func NewOpenCodeSource(cfg *config.Config, ms *state.MonitorState) *OpenCodeSource {
	return &OpenCodeSource{
		config:         cfg,
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
		return fmt.Errorf("opencode db not found: %w", err)
	}
	db, err := sql.Open("sqlite", o.dbPath+"?mode=ro&_journal_mode=wal")
	if err != nil {
		return fmt.Errorf("opening opencode db: %w", err)
	}
	o.db = db
	return nil
}

func (o *OpenCodeSource) DiscoverSessions() []ActiveSession {
	sessionMapPath := filepath.Join(o.config.VoltaDir, "session_map.json")
	sm, err := state.LoadSessionMap(sessionMapPath)
	if err != nil {
		return nil
	}

	// Clean up stale sessions
	for key := range o.lastSessionMap {
		if _, ok := sm[key]; !ok {
			o.monitorState.RemoveSession(key)
		}
	}

	// Try to discover session IDs for entries that don't have one yet
	for key, entry := range sm {
		if entry.SessionID == "" {
			discovered, err := o.discoverSession(entry.CWD)
			if err != nil {
				continue
			}
			entry.SessionID = discovered
			sm[key] = entry

			// Persist discovered session ID
			_ = state.ReadModifyWriteSessionMap(sessionMapPath, func(data map[string]state.SessionMapEntry) {
				if e, ok := data[key]; ok {
					e.SessionID = discovered
					data[key] = e
				}
			})
			log.Printf("OpenCode session discovered: %s -> %s", entry.CWD, discovered)
		}
	}

	var sessions []ActiveSession
	for key, entry := range sm {
		if entry.SessionID == "" {
			continue // not yet discovered
		}
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
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

func (o *OpenCodeSource) discoverSession(directory string) (string, error) {
	if err := o.ensureDB(); err != nil {
		return "", err
	}
	var sessionID string
	err := o.db.QueryRow(
		`SELECT id FROM session WHERE directory = ? ORDER BY time_created DESC LIMIT 1`,
		directory,
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("discovering session for %s: %w", directory, err)
	}
	return sessionID, nil
}

func (o *OpenCodeSource) ReadNewEntries(session ActiveSession, lastOffset int64) ([]ParsedEntry, int64, error) {
	if err := o.ensureDB(); err != nil {
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

		inputStr := ""
		if part.State.Input != nil {
			inputStr = string(part.State.Input)
		}

		return &ParsedEntry{
			ContentType: "tool_use",
			ToolName:    part.Tool,
			ToolInput:   inputStr,
			ToolUseID:   partID,
			Text:        formatOpenCodeToolUse(part.Tool, inputStr),
		}

	case "completed":
		if lastStatus == "completed" {
			return nil
		}
		o.knownTools[partID] = "completed"

		inputStr := ""
		if part.State.Input != nil {
			inputStr = string(part.State.Input)
		}

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

		inputStr := ""
		if part.State.Input != nil {
			inputStr = string(part.State.Input)
		}

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

func formatOpenCodeToolUse(toolName, input string) string {
	if toolName == "" {
		return ""
	}
	return fmt.Sprintf("\U0001f527 %s", toolName)
}

// Close closes the underlying database connection.
func (o *OpenCodeSource) Close() {
	if o.db != nil {
		o.db.Close()
		o.db = nil
	}
}
