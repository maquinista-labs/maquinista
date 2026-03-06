package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/otaviocarvalho/volta/internal/db"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"this-is-too-long", 10, "this-is-to"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	tests := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"seconds", 30 * time.Second, "30s ago"},
		{"minutes", 5 * time.Minute, "5m ago"},
		{"hours", 2 * time.Hour, "2h ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relativeTime(time.Now().Add(-tt.ago))
			if got != tt.want {
				t.Errorf("relativeTime(%v ago) = %q, want %q", tt.ago, got, tt.want)
			}
		})
	}
}

func TestNewModel(t *testing.T) {
	// NewModel should work with a nil pool for testing.
	m := NewModel(nil)
	if m.pool != nil {
		t.Error("expected nil pool in test model")
	}
}

func TestModelView_NoAgents(t *testing.T) {
	m := model{
		agents: nil,
		tasks:  nil,
	}
	view := m.View()
	if view == "" {
		t.Error("expected non-empty view")
	}
}

func TestModelView_WithAgents(t *testing.T) {
	now := time.Now()
	taskID := "test-task"
	m := model{
		agents: []*db.Agent{
			{ID: "agent-1", Status: "working", TaskID: &taskID, LastSeen: &now},
			{ID: "agent-2", Status: "idle", LastSeen: &now},
		},
		tasks: []*db.Task{
			{ID: "task-1", Status: "done"},
			{ID: "task-2", Status: "ready"},
			{ID: "task-3", Status: "claimed"},
		},
	}
	view := m.View()
	if view == "" {
		t.Error("expected non-empty view")
	}
}

func TestModelUpdate_Quit(t *testing.T) {
	m := model{}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Error("expected quit command on 'q' key")
	}
}

func TestTickCmd(t *testing.T) {
	cmd := tickCmd()
	if cmd == nil {
		t.Error("expected non-nil tick command")
	}
}
