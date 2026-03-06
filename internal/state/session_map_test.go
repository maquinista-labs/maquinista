package state

import (
	"path/filepath"
	"testing"
)

func TestSessionMap_LoadWrite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_map.json")

	data := map[string]SessionMapEntry{
		"tramuntana:@1": {SessionID: "sess1", CWD: "/tmp/project", WindowName: "proj"},
	}

	if err := WriteSessionMap(path, data); err != nil {
		t.Fatalf("WriteSessionMap: %v", err)
	}

	loaded, err := LoadSessionMap(path)
	if err != nil {
		t.Fatalf("LoadSessionMap: %v", err)
	}

	entry, ok := loaded["tramuntana:@1"]
	if !ok {
		t.Fatal("expected entry for tramuntana:@1")
	}
	if entry.SessionID != "sess1" {
		t.Errorf("SessionID = %q", entry.SessionID)
	}
	if entry.CWD != "/tmp/project" {
		t.Errorf("CWD = %q", entry.CWD)
	}
}

func TestSessionMap_LoadMissing(t *testing.T) {
	data, err := LoadSessionMap("/nonexistent/session_map.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data == nil {
		t.Error("should return empty map, not nil")
	}
}

func TestReadModifyWriteSessionMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_map.json")

	// Write initial
	err := ReadModifyWriteSessionMap(path, func(data map[string]SessionMapEntry) {
		data["key1"] = SessionMapEntry{SessionID: "s1", CWD: "/a", WindowName: "w1"}
	})
	if err != nil {
		t.Fatalf("ReadModifyWrite (1): %v", err)
	}

	// Modify
	err = ReadModifyWriteSessionMap(path, func(data map[string]SessionMapEntry) {
		data["key2"] = SessionMapEntry{SessionID: "s2", CWD: "/b", WindowName: "w2"}
	})
	if err != nil {
		t.Fatalf("ReadModifyWrite (2): %v", err)
	}

	// Verify
	loaded, err := LoadSessionMap(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Errorf("expected 2 entries, got %d", len(loaded))
	}
}

func TestRemoveSessionMapEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_map.json")

	data := map[string]SessionMapEntry{
		"key1": {SessionID: "s1"},
		"key2": {SessionID: "s2"},
	}
	WriteSessionMap(path, data)

	err := RemoveSessionMapEntry(path, "key1")
	if err != nil {
		t.Fatalf("RemoveSessionMapEntry: %v", err)
	}

	loaded, _ := LoadSessionMap(path)
	if _, ok := loaded["key1"]; ok {
		t.Error("key1 should be removed")
	}
	if _, ok := loaded["key2"]; !ok {
		t.Error("key2 should remain")
	}
}
