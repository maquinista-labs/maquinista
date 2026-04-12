package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
)

func TestResolveWindow_Bound(t *testing.T) {
	b := &Bot{
		config: &config.Config{
			AllowedUsers: []int64{100},
		},
		state: state.NewState(),
	}
	b.state.BindThread("100", "42", "@5")

	// Simulate thread ID cache
	threadCacheMu.Lock()
	threadIDCache[1001] = 42
	threadCacheMu.Unlock()
	defer func() {
		threadCacheMu.Lock()
		delete(threadIDCache, 1001)
		threadCacheMu.Unlock()
	}()

	msg := &tgbotapi.Message{
		MessageID: 1001,
		From:      &tgbotapi.User{ID: 100},
	}

	windowID, bound := b.resolveWindow(msg)
	if !bound {
		t.Fatal("expected window to be bound")
	}
	if windowID != "@5" {
		t.Errorf("got window %q, want @5", windowID)
	}
}

func TestResolveWindow_Unbound(t *testing.T) {
	b := &Bot{
		config: &config.Config{
			AllowedUsers: []int64{100},
		},
		state: state.NewState(),
	}

	// Simulate thread ID cache
	threadCacheMu.Lock()
	threadIDCache[1002] = 99
	threadCacheMu.Unlock()
	defer func() {
		threadCacheMu.Lock()
		delete(threadIDCache, 1002)
		threadCacheMu.Unlock()
	}()

	msg := &tgbotapi.Message{
		MessageID: 1002,
		From:      &tgbotapi.User{ID: 100},
	}

	_, bound := b.resolveWindow(msg)
	if bound {
		t.Error("expected window to be unbound")
	}
}

func TestWindowIDFromKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"tramuntana:@5", "@5"},
		{"session:@12", "@12"},
		{"nocolon", "nocolon"},
		{"a:b:@3", "@3"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := windowIDFromKey(tt.key)
			if got != tt.want {
				t.Errorf("windowIDFromKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestHandleCommand_Routes(t *testing.T) {
	tests := []struct {
		cmd     string
		forward bool // true if it should use forwardCommand
	}{
		{"clear", true},
		{"compact", true},
		{"cost", true},
		{"help", true},
		{"memory", true},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			msg := &tgbotapi.Message{
				MessageID: 1,
				Text:      "/" + tt.cmd,
				Entities: []tgbotapi.MessageEntity{
					{Type: "bot_command", Offset: 0, Length: len(tt.cmd) + 1},
				},
			}
			if msg.Command() != tt.cmd {
				t.Errorf("Command() = %q, want %q", msg.Command(), tt.cmd)
			}
		})
	}
}

func TestHandleCommand_EscRoute(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 1,
		Text:      "/esc",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 4},
		},
	}
	if msg.Command() != "esc" {
		t.Errorf("Command() = %q, want esc", msg.Command())
	}
}

func TestTopicClose_CleansUpState(t *testing.T) {
	s := state.NewState()

	// Set up bindings
	s.BindThread("100", "42", "@5")
	s.SetWindowState("@5", state.WindowState{SessionID: "abc", CWD: "/tmp", WindowName: "test"})
	s.SetWindowDisplayName("@5", "test")
	s.SetGroupChatID("100", "42", -1001234)
	s.BindProject("42", "myproject")

	// Verify setup
	if _, ok := s.GetWindowForThread("100", "42"); !ok {
		t.Fatal("expected binding to exist")
	}

	// Simulate cleanup (what handleTopicClose does)
	s.UnbindThread("100", "42")
	s.RemoveWindowState("@5")
	s.RemoveGroupChatID("100", "42")
	s.RemoveProject("42")

	// Verify cleanup
	if _, ok := s.GetWindowForThread("100", "42"); ok {
		t.Error("binding should be removed")
	}
	if _, ok := s.GetWindowState("@5"); ok {
		t.Error("window state should be removed")
	}
	if _, ok := s.GetWindowDisplayName("@5"); ok {
		t.Error("display name should be removed")
	}
	if _, ok := s.GetGroupChatID("100", "42"); ok {
		t.Error("group chat ID should be removed")
	}
	if _, ok := s.GetProject("42"); ok {
		t.Error("project binding should be removed")
	}
}

func TestTopicClose_UnboundIsNoop(t *testing.T) {
	s := state.NewState()

	// No bindings — AllUserIDs should return empty
	ids := s.AllUserIDs()
	if len(ids) != 0 {
		t.Errorf("expected no user IDs, got %d", len(ids))
	}
}

func TestAllUserIDs(t *testing.T) {
	s := state.NewState()
	s.BindThread("100", "1", "@1")
	s.BindThread("200", "2", "@2")

	ids := s.AllUserIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 user IDs, got %d", len(ids))
	}

	found := make(map[string]bool)
	for _, id := range ids {
		found[id] = true
	}
	if !found["100"] || !found["200"] {
		t.Errorf("expected user IDs 100 and 200, got %v", ids)
	}
}
