package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

// OpenClaudeSource implements TranscriptSource for OpenClaude, a
// Claude-compatible fork. Session discovery flows through the same
// DB-backed path as ClaudeSource (Phase A of
// plans/active/json-state-migration.md).
type OpenClaudeSource struct {
	config         *config.Config
	pool           *pgxpool.Pool
	appState       *state.State
	monitorState   *state.MonitorState
	pendingTools   map[string]PendingTool
	fileMtimes     map[string]time.Time
	lastSessionMap map[string]state.SessionMapEntry
}

// NewOpenClaudeSource creates a new OpenClaudeSource.
func NewOpenClaudeSource(cfg *config.Config, pool *pgxpool.Pool, st *state.State, ms *state.MonitorState) *OpenClaudeSource {
	return &OpenClaudeSource{
		config:         cfg,
		pool:           pool,
		appState:       st,
		monitorState:   ms,
		pendingTools:   make(map[string]PendingTool),
		fileMtimes:     make(map[string]time.Time),
		lastSessionMap: make(map[string]state.SessionMapEntry),
	}
}

func (o *OpenClaudeSource) Name() string {
	return "openclaude"
}

func (o *OpenClaudeSource) DiscoverSessions() []ActiveSession {
	if o.pool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sm, err := loadRunnerSessionMap(ctx, o.pool, "openclaude")
	if err != nil {
		log.Printf("openclaude source: session discovery: %v", err)
		return nil
	}

	for key := range o.lastSessionMap {
		if _, ok := sm[key]; !ok {
			o.monitorState.RemoveSession(key)
			delete(o.fileMtimes, key)
		}
	}

	var sessions []ActiveSession
	for key := range sm {
		windowID := windowIDFromSessionKey(key)
		if windowID == "" {
			continue
		}
		if o.appState.GetWindowRunner(windowID) != "openclaude" {
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

func (o *OpenClaudeSource) ReadNewEntries(session ActiveSession, lastOffset int64) ([]ParsedEntry, int64, error) {
	entry, ok := o.lastSessionMap[session.Key]
	if !ok {
		return nil, lastOffset, nil
	}

	// Find the JSONL file for this session
	jsonlPath := o.findJSONLFile(entry.SessionID, entry.CWD)
	if jsonlPath == "" {
		return nil, lastOffset, nil
	}

	// Check mtime
	if !o.hasFileChanged(jsonlPath) {
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
			o.monitorState.UpdateOffset(session.Key, entry.SessionID, jsonlPath, newOffset)
		}
		return nil, newOffset, nil
	}

	// Parse entries with tool pairing
	parsed := ParseEntries(entries, o.pendingTools)

	// Update offset in monitor state
	o.monitorState.UpdateOffset(session.Key, entry.SessionID, jsonlPath, newOffset)

	return parsed, newOffset, nil
}

func (o *OpenClaudeSource) ExtractStatusLine(paneText string) (string, bool) {
	return ExtractStatusLine(paneText)
}

func (o *OpenClaudeSource) IsInteractiveUI(paneText string) bool {
	return IsInteractiveUI(paneText)
}

// hasFileChanged checks if a file's mtime has changed since last check.
func (o *OpenClaudeSource) hasFileChanged(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	mtime := info.ModTime()
	lastMtime, ok := o.fileMtimes[path]
	if ok && mtime.Equal(lastMtime) {
		return false
	}

	o.fileMtimes[path] = mtime
	return true
}

// findJSONLFile locates the JSONL transcript file for a session.
// OpenClaude stores transcripts in the same ~/.claude/projects/ directory as Claude Code.
func (o *OpenClaudeSource) findJSONLFile(sessionID, cwd string) string {
	// First: check monitor state for cached path
	for _, key := range o.monitorState.AllKeys() {
		tracked, ok := o.monitorState.GetTracked(key)
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
		if path := o.searchSessionsIndex(indexPath, sessionID, projectDir); path != "" {
			return path
		}

		// Fallback: glob for JSONL files
		if path := o.searchJSONLFiles(projectDir, sessionID); path != "" {
			return path
		}
	}

	return ""
}

func (o *OpenClaudeSource) searchSessionsIndex(indexPath, sessionID, projectDir string) string {
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

func (o *OpenClaudeSource) searchJSONLFiles(projectDir, sessionID string) string {
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
