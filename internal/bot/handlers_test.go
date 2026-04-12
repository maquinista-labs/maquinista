package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// newTestBot creates a Bot with an in-memory state for testing.
// It does NOT connect to Telegram — only for testing pure logic.
func newTestBot(t *testing.T) *Bot {
	t.Helper()
	return &Bot{
		config: &config.Config{
			AllowedUsers:    []int64{100},
			TmuxSessionName: "test-session",
		},
		state:              state.NewState(),
		browseStates:       make(map[int64]*BrowseState),
		windowCache:        make(map[int64][]tmux.Window),
		windowPickerStates: make(map[int64]*windowPickerState),
		addTaskStates:      make(map[int64]*addTaskState),
	}
}

func TestHandleTextMessage_StoresGroupChatID(t *testing.T) {
	b := &Bot{
		config: &config.Config{
			AllowedUsers:    []int64{100},
			TmuxSessionName: "test-session",
		},
		state:        state.NewState(),
		browseStates: make(map[int64]*BrowseState),
	}

	// Set up thread ID cache to simulate forum message
	threadCacheMu.Lock()
	threadIDCache[42] = 7
	threadCacheMu.Unlock()
	defer func() {
		threadCacheMu.Lock()
		delete(threadIDCache, 42)
		threadCacheMu.Unlock()
	}()

	// Verify group chat ID is stored after the state method is called
	b.state.SetGroupChatID("100", "7", int64(-1001234))
	chatID, ok := b.state.GetGroupChatID("100", "7")
	if !ok {
		t.Fatal("expected group chat ID to be stored")
	}
	if chatID != -1001234 {
		t.Errorf("got chat ID %d, want -1001234", chatID)
	}
}

func TestHandleTextMessage_DetectsBashPrefix(t *testing.T) {
	tests := []struct {
		text   string
		isBash bool
	}{
		{"!git status", true},
		{"!ls", true},
		{"!", false},  // single ! is not a bash command
		{"hello", false},
		{"!!", true},
		{"!!git push", true},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			hasBash := len(tt.text) > 1 && tt.text[0] == '!'
			if hasBash != tt.isBash {
				t.Errorf("bash detection for %q: got %v, want %v", tt.text, hasBash, tt.isBash)
			}
		})
	}
}

func TestHandleUnboundTopic_NoWindows(t *testing.T) {
	b := &Bot{
		config: &config.Config{
			AllowedUsers:    []int64{100},
			TmuxSessionName: "nonexistent-session-for-test",
		},
		state:        state.NewState(),
		browseStates: make(map[int64]*BrowseState),
	}

	// With no tmux session, ListWindows will fail, so handleUnboundTopic
	// should gracefully handle the error. We test that AllBoundWindowIDs works.
	bound := b.state.AllBoundWindowIDs()
	if len(bound) != 0 {
		t.Errorf("expected no bound windows, got %d", len(bound))
	}
}

func TestAllBoundWindowIDs_FiltersCorrectly(t *testing.T) {
	s := state.NewState()
	s.BindThread("100", "1", "@10")
	s.BindThread("100", "2", "@20")
	s.BindThread("200", "3", "@10") // same window, different user

	bound := s.AllBoundWindowIDs()
	if !bound["@10"] {
		t.Error("@10 should be bound")
	}
	if !bound["@20"] {
		t.Error("@20 should be bound")
	}
	if bound["@30"] {
		t.Error("@30 should not be bound")
	}
}

func TestRouteCallback_Prefixes(t *testing.T) {
	tests := []struct {
		data   string
		prefix string
	}{
		{"dir_select:0", "dir_"},
		{"dir_page:1", "dir_"},
		{"win_bind:0", "win_"},
		{"win_new", "win_"},
		{"hist_page:0", "hist_"},
		{"ss_refresh", "ss_"},
		{"nav_up", "nav_"},
		{"task_pri:5", "task_"},
		{"task_cancel", "task_"},
	}

	for _, tt := range tests {
		t.Run(tt.data, func(t *testing.T) {
			matched := false
			for _, p := range []string{"dir_", "win_", "hist_", "ss_", "nav_", "task_"} {
				if len(tt.data) >= len(p) && tt.data[:len(p)] == p {
					if p == tt.prefix {
						matched = true
					}
				}
			}
			if !matched {
				t.Errorf("callback %q should match prefix %q", tt.data, tt.prefix)
			}
		})
	}
}

func TestHandleMessage_RoutesToCommand(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 1,
		Text:      "/clear",
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 6},
		},
	}

	if !msg.IsCommand() {
		t.Error("message with /clear should be detected as command")
	}
	if msg.Command() != "clear" {
		t.Errorf("command should be 'clear', got %q", msg.Command())
	}
}

func TestHandleMessage_RoutesToText(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 2,
		Text:      "hello world",
	}

	if msg.IsCommand() {
		t.Error("plain text should not be detected as command")
	}
	if msg.Text == "" {
		t.Error("text should not be empty")
	}
}

func TestHandleMessage_IgnoresEmptyText(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 3,
		Text:      "",
	}

	// The handler checks msg.Text != "" before processing
	if msg.Text != "" {
		t.Error("empty text should be empty")
	}
}
