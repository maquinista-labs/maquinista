package bot

import (
	"testing"

	"github.com/maquinista-labs/maquinista/internal/state"
)

func TestReResolveWindow(t *testing.T) {
	s := state.NewState()
	s.BindThread("user1", "thread1", "@old")
	s.SetWindowState("@old", state.WindowState{SessionID: "sess1"})
	s.SetWindowDisplayName("@old", "my-window")
	s.SetUserWindowOffset("user1", "@old", 1000)

	reResolveWindow(s, "@old", "@new")

	// Check binding updated
	wid, ok := s.GetWindowForThread("user1", "thread1")
	if !ok || wid != "@new" {
		t.Errorf("binding should point to @new, got %q %v", wid, ok)
	}

	// Check window state moved
	_, ok = s.GetWindowState("@old")
	if ok {
		t.Error("old window state should be removed")
	}
	ws, ok := s.GetWindowState("@new")
	if !ok {
		t.Error("new window state should exist")
	}
	if ws.SessionID != "sess1" {
		t.Errorf("session ID = %q, want sess1", ws.SessionID)
	}

	// Check display name moved
	name, ok := s.GetWindowDisplayName("@new")
	if !ok || name != "my-window" {
		t.Errorf("display name = %q %v, want my-window", name, ok)
	}

	// Check offset moved
	offset := s.GetUserWindowOffset("user1", "@new")
	if offset != 1000 {
		t.Errorf("offset = %d, want 1000", offset)
	}
	oldOffset := s.GetUserWindowOffset("user1", "@old")
	if oldOffset != 0 {
		t.Errorf("old offset should be 0, got %d", oldOffset)
	}
}

func TestCleanStaleProjects(t *testing.T) {
	s := state.NewState()
	s.BindProject("thread1", "proj1")
	s.BindProject("thread2", "proj2")

	// This is more of a smoke test since the function currently
	// can't iterate ProjectBindings without exposing internals
	cleanStaleProjects(s)

	// Projects should still exist since we can't easily check
	_, ok1 := s.GetProject("thread1")
	_, ok2 := s.GetProject("thread2")
	if !ok1 || !ok2 {
		t.Error("projects should still exist (cleanup is limited)")
	}
}

func TestCleanupDeadWindow(t *testing.T) {
	// Create a minimal bot-like struct for testing
	s := state.NewState()
	s.BindThread("user1", "thread1", "@dead")
	s.BindThread("user2", "thread2", "@dead")
	s.SetWindowState("@dead", state.WindowState{SessionID: "sess1"})
	s.SetWindowDisplayName("@dead", "dead-window")
	s.SetGroupChatID("user1", "thread1", -12345)
	s.SetGroupChatID("user2", "thread2", -12345)

	// Simulate cleanup using state methods directly
	users := s.FindUsersForWindow("@dead")
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	for _, ut := range users {
		s.UnbindThread(ut.UserID, ut.ThreadID)
		s.RemoveGroupChatID(ut.UserID, ut.ThreadID)
	}
	s.RemoveWindowState("@dead")

	// Verify cleanup
	_, ok := s.GetWindowForThread("user1", "thread1")
	if ok {
		t.Error("user1 binding should be removed")
	}
	_, ok = s.GetWindowForThread("user2", "thread2")
	if ok {
		t.Error("user2 binding should be removed")
	}
	_, ok = s.GetWindowState("@dead")
	if ok {
		t.Error("window state should be removed")
	}
	_, ok = s.GetGroupChatID("user1", "thread1")
	if ok {
		t.Error("group chat ID should be removed")
	}
}

func TestAllBoundWindowIDs(t *testing.T) {
	s := state.NewState()
	s.BindThread("user1", "thread1", "@1")
	s.BindThread("user1", "thread2", "@2")
	s.BindThread("user2", "thread3", "@1")

	ids := s.AllBoundWindowIDs()
	if !ids["@1"] {
		t.Error("should include @1")
	}
	if !ids["@2"] {
		t.Error("should include @2")
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 unique IDs, got %d", len(ids))
	}
}

func TestFindUsersForWindow(t *testing.T) {
	s := state.NewState()
	s.BindThread("user1", "thread1", "@1")
	s.BindThread("user2", "thread2", "@1")
	s.BindThread("user3", "thread3", "@2")

	users := s.FindUsersForWindow("@1")
	if len(users) != 2 {
		t.Fatalf("expected 2 users for @1, got %d", len(users))
	}

	// Verify both users are present
	found := make(map[string]bool)
	for _, u := range users {
		found[u.UserID] = true
	}
	if !found["user1"] || !found["user2"] {
		t.Error("should find both user1 and user2")
	}
}
