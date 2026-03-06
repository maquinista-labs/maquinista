package state

import (
	"path/filepath"
	"testing"
)

func TestMonitorState_NewEmpty(t *testing.T) {
	ms := NewMonitorState()
	if ms.TrackedSessions == nil {
		t.Error("should be initialized")
	}
	if ms.IsDirty() {
		t.Error("new state should not be dirty")
	}
}

func TestMonitorState_UpdateAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor_state.json")

	ms := NewMonitorState()
	ms.UpdateOffset("key1", "sess1", "/path/to/file.jsonl", 1024)

	if !ms.IsDirty() {
		t.Error("should be dirty after update")
	}

	ts, ok := ms.GetTracked("key1")
	if !ok {
		t.Fatal("key1 not found")
	}
	if ts.SessionID != "sess1" || ts.LastByteOffset != 1024 {
		t.Errorf("unexpected: %+v", ts)
	}

	if err := ms.SaveIfDirty(path); err != nil {
		t.Fatalf("SaveIfDirty: %v", err)
	}

	if ms.IsDirty() {
		t.Error("should not be dirty after save")
	}

	// SaveIfDirty again should be a no-op
	if err := ms.SaveIfDirty(path); err != nil {
		t.Fatalf("SaveIfDirty (no-op): %v", err)
	}
}

func TestMonitorState_LoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor_state.json")

	ms := NewMonitorState()
	ms.UpdateOffset("key1", "sess1", "/file.jsonl", 2048)
	ms.ForceSave(path)

	loaded, err := LoadMonitorState(path)
	if err != nil {
		t.Fatalf("LoadMonitorState: %v", err)
	}

	ts, ok := loaded.GetTracked("key1")
	if !ok || ts.LastByteOffset != 2048 {
		t.Errorf("expected 2048, got %+v", ts)
	}
}

func TestMonitorState_RemoveSession(t *testing.T) {
	ms := NewMonitorState()
	ms.UpdateOffset("key1", "s1", "/f.jsonl", 100)
	ms.dirty = false // reset

	ms.RemoveSession("key1")
	if !ms.IsDirty() {
		t.Error("should be dirty after remove")
	}

	_, ok := ms.GetTracked("key1")
	if ok {
		t.Error("key1 should be removed")
	}

	// Remove non-existent should not mark dirty again
	ms.dirty = false
	ms.RemoveSession("nonexistent")
	if ms.IsDirty() {
		t.Error("should not be dirty after removing non-existent")
	}
}

func TestMonitorState_AllKeys(t *testing.T) {
	ms := NewMonitorState()
	ms.UpdateOffset("a", "s1", "/a.jsonl", 0)
	ms.UpdateOffset("b", "s2", "/b.jsonl", 0)

	keys := ms.AllKeys()
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(keys))
	}
}

func TestMonitorState_LoadMissing(t *testing.T) {
	ms, err := LoadMonitorState("/nonexistent/monitor_state.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms.TrackedSessions == nil {
		t.Error("should be initialized")
	}
}
