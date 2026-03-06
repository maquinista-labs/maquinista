package state

import (
	"sync"
)

// TrackedSession tracks byte offset for a JSONL session file.
type TrackedSession struct {
	SessionID      string `json:"session_id"`
	FilePath       string `json:"file_path"`
	LastByteOffset int64  `json:"last_byte_offset"`
}

// MonitorState tracks all monitored sessions with byte offsets.
type MonitorState struct {
	mu              sync.Mutex
	TrackedSessions map[string]TrackedSession `json:"tracked_sessions"`
	dirty           bool
}

// NewMonitorState creates a new empty MonitorState.
func NewMonitorState() *MonitorState {
	return &MonitorState{
		TrackedSessions: make(map[string]TrackedSession),
	}
}

// LoadMonitorState reads monitor state from a JSON file.
func LoadMonitorState(path string) (*MonitorState, error) {
	ms := NewMonitorState()
	if err := loadJSON(path, ms); err != nil {
		return nil, err
	}
	if ms.TrackedSessions == nil {
		ms.TrackedSessions = make(map[string]TrackedSession)
	}
	return ms, nil
}

// SaveIfDirty saves the monitor state only if it has been modified.
func (ms *MonitorState) SaveIfDirty(path string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if !ms.dirty {
		return nil
	}
	if err := atomicWriteJSON(path, ms); err != nil {
		return err
	}
	ms.dirty = false
	return nil
}

// ForceSave saves the monitor state regardless of dirty flag.
func (ms *MonitorState) ForceSave(path string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if err := atomicWriteJSON(path, ms); err != nil {
		return err
	}
	ms.dirty = false
	return nil
}

// UpdateOffset updates the byte offset for a tracked session.
func (ms *MonitorState) UpdateOffset(key string, sessionID, filePath string, offset int64) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.TrackedSessions[key] = TrackedSession{
		SessionID:      sessionID,
		FilePath:       filePath,
		LastByteOffset: offset,
	}
	ms.dirty = true
}

// GetTracked returns a tracked session by key.
func (ms *MonitorState) GetTracked(key string) (TrackedSession, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ts, ok := ms.TrackedSessions[key]
	return ts, ok
}

// RemoveSession removes a tracked session.
func (ms *MonitorState) RemoveSession(key string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.TrackedSessions[key]; ok {
		delete(ms.TrackedSessions, key)
		ms.dirty = true
	}
}

// AllKeys returns all tracked session keys.
func (ms *MonitorState) AllKeys() []string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	keys := make([]string, 0, len(ms.TrackedSessions))
	for k := range ms.TrackedSessions {
		keys = append(keys, k)
	}
	return keys
}

// IsDirty returns whether the state has been modified since last save.
func (ms *MonitorState) IsDirty() bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.dirty
}
