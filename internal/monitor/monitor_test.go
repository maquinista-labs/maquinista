package monitor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

func TestWindowIDFromSessionKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"tramuntana:@5", "@5"},
		{"session:@12", "@12"},
		{"a:b:@3", "@3"},
		{"nowindow", ""},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := windowIDFromSessionKey(tt.key)
			if got != tt.want {
				t.Errorf("windowIDFromSessionKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestMonitorNew(t *testing.T) {
	cfg := &config.Config{
		MaquinistaDir:            t.TempDir(),
		MonitorPollInterval: 2.0,
	}
	st := state.NewState()
	ms := state.NewMonitorState()

	m := New(cfg, st, ms, nil)
	if m == nil {
		t.Fatal("monitor should not be nil")
	}
	if m.pollInterval.Seconds() != 2.0 {
		t.Errorf("poll interval = %v, want 2s", m.pollInterval)
	}
}

func TestClaudeSource_HasFileChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	os.WriteFile(path, []byte(`{}`), 0o644)

	cfg := &config.Config{
		MaquinistaDir:            dir,
		MonitorPollInterval: 2.0,
	}
	cs := NewClaudeSource(cfg, state.NewState(), state.NewMonitorState())

	// First check should return true
	if !cs.hasFileChanged(path) {
		t.Error("first check should detect change")
	}

	// Second check without modification should return false
	if cs.hasFileChanged(path) {
		t.Error("unchanged file should not detect change")
	}

	// Modify file — sleep to ensure different mtime (filesystem may have 1s granularity)
	time.Sleep(10 * time.Millisecond)
	now := time.Now().Add(1 * time.Second)
	os.WriteFile(path, []byte(`{"updated":true}`), 0o644)
	os.Chtimes(path, now, now)
	if !cs.hasFileChanged(path) {
		t.Error("modified file should detect change")
	}
}

func TestClaudeSource_HasFileChanged_NonExistent(t *testing.T) {
	cfg := &config.Config{
		MaquinistaDir:            t.TempDir(),
		MonitorPollInterval: 2.0,
	}
	cs := NewClaudeSource(cfg, state.NewState(), state.NewMonitorState())

	if cs.hasFileChanged("/nonexistent/file.jsonl") {
		t.Error("nonexistent file should not detect change")
	}
}

func TestClaudeSource_ReadNewEntries_Truncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// Write initial content
	os.WriteFile(path, []byte(`{"type":"assistant","message":{"content":"hello"}}`+"\n"), 0o644)

	cfg := &config.Config{
		MaquinistaDir:            dir,
		MonitorPollInterval: 2.0,
	}
	ms := state.NewMonitorState()

	// Set offset beyond file size to simulate /clear truncation
	ms.UpdateOffset("test:@1", "test-session", path, 99999)

	cs := NewClaudeSource(cfg, state.NewState(), ms)
	// Populate lastSessionMap so ReadNewEntries can find the entry
	cs.lastSessionMap = map[string]state.SessionMapEntry{
		"test:@1": {SessionID: "test-session", CWD: dir},
	}

	// ReadNewEntries should reset offset and not crash
	_, newOffset, err := cs.ReadNewEntries(ActiveSession{Key: "test:@1", WindowID: "@1"}, 99999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Offset should be updated to actual content size
	tracked, ok := ms.GetTracked("test:@1")
	if !ok {
		t.Fatal("should have tracked session")
	}
	if tracked.LastByteOffset == 99999 {
		t.Error("offset should have been reset from 99999")
	}
	if newOffset == 99999 {
		t.Error("returned offset should have been reset from 99999")
	}
}

func TestClaudeSource_DiscoverSessions_RemovesStale(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		MaquinistaDir:            dir,
		MonitorPollInterval: 2.0,
	}
	ms := state.NewMonitorState()
	ms.UpdateOffset("old:@1", "old", "/some/path", 100)

	cs := NewClaudeSource(cfg, state.NewState(), ms)
	cs.lastSessionMap = map[string]state.SessionMapEntry{
		"old:@1": {SessionID: "old"},
	}

	// Write an empty session_map.json
	os.WriteFile(filepath.Join(dir, "session_map.json"), []byte(`{}`), 0o644)

	// DiscoverSessions with empty map should clean up stale entries
	cs.DiscoverSessions()

	if _, ok := ms.GetTracked("old:@1"); ok {
		t.Error("stale session should be removed")
	}
}

func TestClaudeSource_SearchSessionsIndex(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME not set")
	}

	// Create mock Claude projects dir
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "test-project")
	os.MkdirAll(projectDir, 0o755)

	// Create sessions-index.json
	indexContent := `{"test-session-id": {"created": "2024-01-01"}}`
	os.WriteFile(filepath.Join(projectDir, "sessions-index.json"), []byte(indexContent), 0o644)

	// Create JSONL file
	os.WriteFile(filepath.Join(projectDir, "test-session-id.jsonl"), []byte(`{}`), 0o644)

	cfg := &config.Config{
		MaquinistaDir:            t.TempDir(),
		MonitorPollInterval: 2.0,
	}
	cs := NewClaudeSource(cfg, state.NewState(), state.NewMonitorState())

	path := cs.searchSessionsIndex(
		filepath.Join(projectDir, "sessions-index.json"),
		"test-session-id",
		projectDir,
	)
	if path == "" {
		t.Error("should find JSONL file from sessions-index")
	}
}

func TestClaudeSource_SearchJSONLFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "abc-123.jsonl"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "other.jsonl"), []byte(`{}`), 0o644)

	cfg := &config.Config{
		MaquinistaDir:            t.TempDir(),
		MonitorPollInterval: 2.0,
	}
	cs := NewClaudeSource(cfg, state.NewState(), state.NewMonitorState())

	path := cs.searchJSONLFiles(dir, "abc-123")
	if path == "" {
		t.Error("should find JSONL file by name")
	}

	path = cs.searchJSONLFiles(dir, "nonexistent")
	if path != "" {
		t.Error("should not find nonexistent session")
	}
}
