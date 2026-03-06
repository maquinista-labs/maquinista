package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewState(t *testing.T) {
	s := NewState()
	if s.ThreadBindings == nil || s.WindowStates == nil || s.GroupChatIDs == nil {
		t.Error("NewState should initialize all maps")
	}
}

func TestLoadSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := NewState()
	s.BindThread("user1", "thread1", "@1")
	s.SetWindowState("@1", WindowState{SessionID: "sess1", CWD: "/tmp", WindowName: "win1"})
	s.SetGroupChatID("user1", "thread1", -100123)
	s.BindProject("thread1", "myproject")
	s.SetWindowDisplayName("@1", "My Window")
	s.SetUserWindowOffset("user1", "@1", 42)

	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wid, ok := loaded.GetWindowForThread("user1", "thread1")
	if !ok || wid != "@1" {
		t.Errorf("GetWindowForThread = %q, %v", wid, ok)
	}

	ws, ok := loaded.GetWindowState("@1")
	if !ok || ws.SessionID != "sess1" {
		t.Errorf("GetWindowState = %v, %v", ws, ok)
	}

	chatID, ok := loaded.GetGroupChatID("user1", "thread1")
	if !ok || chatID != -100123 {
		t.Errorf("GetGroupChatID = %d, %v", chatID, ok)
	}

	proj, ok := loaded.GetProject("thread1")
	if !ok || proj != "myproject" {
		t.Errorf("GetProject = %q, %v", proj, ok)
	}

	name, ok := loaded.GetWindowDisplayName("@1")
	if !ok || name != "My Window" {
		t.Errorf("GetWindowDisplayName = %q, %v", name, ok)
	}

	offset := loaded.GetUserWindowOffset("user1", "@1")
	if offset != 42 {
		t.Errorf("GetUserWindowOffset = %d, want 42", offset)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	s, err := Load("/nonexistent/path/state.json")
	if err != nil {
		t.Fatalf("Load non-existent: %v", err)
	}
	if s.ThreadBindings == nil {
		t.Error("maps should be initialized even from missing file")
	}
}

func TestBindUnbindThread(t *testing.T) {
	s := NewState()
	s.BindThread("u1", "t1", "@1")
	s.BindThread("u1", "t2", "@2")
	s.BindThread("u2", "t1", "@1")

	wid, ok := s.GetWindowForThread("u1", "t1")
	if !ok || wid != "@1" {
		t.Errorf("expected @1, got %q", wid)
	}

	// Find users for window
	users := s.FindUsersForWindow("@1")
	if len(users) != 2 {
		t.Errorf("expected 2 users for @1, got %d", len(users))
	}

	// Unbind
	s.UnbindThread("u1", "t1")
	_, ok = s.GetWindowForThread("u1", "t1")
	if ok {
		t.Error("expected unbound")
	}

	// u1 still has t2
	wid, ok = s.GetWindowForThread("u1", "t2")
	if !ok || wid != "@2" {
		t.Errorf("expected @2, got %q", wid)
	}
}

func TestRemoveWindowState(t *testing.T) {
	s := NewState()
	s.SetWindowState("@1", WindowState{SessionID: "s1"})
	s.SetWindowDisplayName("@1", "name")
	s.SetUserWindowOffset("u1", "@1", 100)

	s.RemoveWindowState("@1")

	_, ok := s.GetWindowState("@1")
	if ok {
		t.Error("window state should be removed")
	}
	_, ok = s.GetWindowDisplayName("@1")
	if ok {
		t.Error("display name should be removed")
	}
	if s.GetUserWindowOffset("u1", "@1") != 0 {
		t.Error("offset should be removed")
	}
}

func TestGroupChatIDs(t *testing.T) {
	s := NewState()
	s.SetGroupChatID("u1", "t1", -100)

	id, ok := s.GetGroupChatID("u1", "t1")
	if !ok || id != -100 {
		t.Errorf("expected -100, got %d", id)
	}

	s.RemoveGroupChatID("u1", "t1")
	_, ok = s.GetGroupChatID("u1", "t1")
	if ok {
		t.Error("should be removed")
	}
}

func TestProjectBindings(t *testing.T) {
	s := NewState()
	s.BindProject("t1", "proj1")

	p, ok := s.GetProject("t1")
	if !ok || p != "proj1" {
		t.Errorf("expected proj1, got %q", p)
	}

	s.RemoveProject("t1")
	_, ok = s.GetProject("t1")
	if ok {
		t.Error("should be removed")
	}
}

func TestAllBoundWindowIDs(t *testing.T) {
	s := NewState()
	s.BindThread("u1", "t1", "@1")
	s.BindThread("u2", "t2", "@2")
	s.BindThread("u1", "t3", "@1")

	bound := s.AllBoundWindowIDs()
	if len(bound) != 2 {
		t.Errorf("expected 2 unique windows, got %d", len(bound))
	}
	if !bound["@1"] || !bound["@2"] {
		t.Errorf("expected @1 and @2, got %v", bound)
	}
}

func TestAtomicWriteJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	data := map[string]string{"key": "value"}
	if err := atomicWriteJSON(path, data); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(content) == 0 {
		t.Error("file should not be empty")
	}
}
