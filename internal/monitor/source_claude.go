package monitor

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// ClaudeSource implements TranscriptSource for Claude Code.
// It discovers sessions via session_map.json and reads JSONL transcript files.
type ClaudeSource struct {
	config         *config.Config
	appState       *state.State
	monitorState   *state.MonitorState
	pendingTools   map[string]PendingTool
	fileMtimes     map[string]time.Time
	lastSessionMap map[string]state.SessionMapEntry
}

// NewClaudeSource creates a new ClaudeSource.
func NewClaudeSource(cfg *config.Config, st *state.State, ms *state.MonitorState) *ClaudeSource {
	return &ClaudeSource{
		config:         cfg,
		appState:       st,
		monitorState:   ms,
		pendingTools:   make(map[string]PendingTool),
		fileMtimes:     make(map[string]time.Time),
		lastSessionMap: make(map[string]state.SessionMapEntry),
	}
}

func (c *ClaudeSource) Name() string {
	return "claude"
}

func (c *ClaudeSource) DiscoverSessions() []ActiveSession {
	sessionMapPath := filepath.Join(c.config.MaquinistaDir, "session_map.json")
	sm, err := state.LoadSessionMap(sessionMapPath)
	if err != nil {
		return nil
	}

	// Detect changes: clean up stale sessions
	for key := range c.lastSessionMap {
		if _, ok := sm[key]; !ok {
			c.monitorState.RemoveSession(key)
			delete(c.fileMtimes, key)
		}
	}

	var sessions []ActiveSession
	for key := range sm {
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}
		// Only claim sessions owned by this source
		if c.appState.GetWindowRunner(windowID) != "claude" {
			continue
		}
		sessions = append(sessions, ActiveSession{
			Key:      key,
			WindowID: windowID,
		})
	}

	c.lastSessionMap = sm
	return sessions
}

func (c *ClaudeSource) ReadNewEntries(session ActiveSession, lastOffset int64) ([]ParsedEntry, int64, error) {
	entry, ok := c.lastSessionMap[session.Key]
	if !ok {
		return nil, lastOffset, nil
	}

	// Find the JSONL file for this session
	jsonlPath := c.findJSONLFile(entry.SessionID, entry.CWD)
	if jsonlPath == "" {
		return nil, lastOffset, nil
	}

	// Check mtime
	if !c.hasFileChanged(jsonlPath) {
		return nil, lastOffset, nil
	}

	// Check file size (detect truncation from /clear)
	info, err := os.Stat(jsonlPath)
	if err != nil {
		return nil, lastOffset, nil
	}
	offset := lastOffset
	if offset > info.Size() {
		offset = 0 // file was truncated
	}

	// Open and read new content
	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, lastOffset, nil
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil, lastOffset, nil
		}
	}

	var entries []*Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer
	var bytesRead int64

	for scanner.Scan() {
		line := scanner.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for newline

		e, err := ParseLine(line)
		if err != nil {
			log.Printf("JSONL parse error at offset %d: %v", offset+bytesRead, err)
			continue
		}
		if e != nil {
			entries = append(entries, e)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("JSONL read error for %s at offset %d: %v (not advancing offset)", jsonlPath, offset+bytesRead, err)
		return nil, lastOffset, nil // don't advance offset — will re-read on next poll
	}

	newOffset := offset + bytesRead

	if len(entries) == 0 {
		// Update offset even if no entries (skip empty lines)
		if bytesRead > 0 {
			c.monitorState.UpdateOffset(session.Key, entry.SessionID, jsonlPath, newOffset)
		}
		return nil, newOffset, nil
	}

	// Parse entries with tool pairing
	parsed := ParseEntries(entries, c.pendingTools)

	// Update offset in monitor state
	c.monitorState.UpdateOffset(session.Key, entry.SessionID, jsonlPath, newOffset)

	return parsed, newOffset, nil
}

func (c *ClaudeSource) ExtractStatusLine(paneText string) (string, bool) {
	return ExtractStatusLine(paneText)
}

func (c *ClaudeSource) IsInteractiveUI(paneText string) bool {
	return IsInteractiveUI(paneText)
}

// hasFileChanged checks if a file's mtime has changed since last check.
func (c *ClaudeSource) hasFileChanged(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	mtime := info.ModTime()
	lastMtime, ok := c.fileMtimes[path]
	if ok && mtime.Equal(lastMtime) {
		return false
	}

	c.fileMtimes[path] = mtime
	return true
}

// findJSONLFile locates the JSONL transcript file for a session.
func (c *ClaudeSource) findJSONLFile(sessionID, cwd string) string {
	// First: check monitor state for cached path
	for _, key := range c.monitorState.AllKeys() {
		tracked, ok := c.monitorState.GetTracked(key)
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
		if path := c.searchSessionsIndex(indexPath, sessionID, projectDir); path != "" {
			return path
		}

		// Fallback: glob for JSONL files
		if path := c.searchJSONLFiles(projectDir, sessionID); path != "" {
			return path
		}
	}

	return ""
}

func (c *ClaudeSource) searchSessionsIndex(indexPath, sessionID, projectDir string) string {
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

func (c *ClaudeSource) searchJSONLFiles(projectDir, sessionID string) string {
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
