package bot

import (
	"testing"

	"github.com/maquinista-labs/maquinista/internal/monitor"
)

func TestBuildInteractiveKeyboard_Full(t *testing.T) {
	kb := buildInteractiveKeyboard("ExitPlanMode")
	if len(kb.InlineKeyboard) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(kb.InlineKeyboard))
	}
	// Row 1: arrow keys (4 buttons)
	if len(kb.InlineKeyboard[0]) != 4 {
		t.Errorf("row 1 should have 4 buttons, got %d", len(kb.InlineKeyboard[0]))
	}
	// Row 2: Space, Tab, Esc, Enter (4 buttons)
	if len(kb.InlineKeyboard[1]) != 4 {
		t.Errorf("row 2 should have 4 buttons, got %d", len(kb.InlineKeyboard[1]))
	}
	// Row 3: Refresh (1 button)
	if len(kb.InlineKeyboard[2]) != 1 {
		t.Errorf("row 3 should have 1 button, got %d", len(kb.InlineKeyboard[2]))
	}
}

func TestBuildInteractiveKeyboard_RestoreCheckpoint(t *testing.T) {
	kb := buildInteractiveKeyboard("RestoreCheckpoint")
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("expected 2 rows for RestoreCheckpoint, got %d", len(kb.InlineKeyboard))
	}
	// Row 1: Up, Down only
	if len(kb.InlineKeyboard[0]) != 2 {
		t.Errorf("row 1 should have 2 buttons, got %d", len(kb.InlineKeyboard[0]))
	}
}

func TestFormatInteractiveContent(t *testing.T) {
	tests := []struct {
		name     string
		uiName   string
		wantName string
	}{
		{"exit plan", "ExitPlanMode", "Plan Review"},
		{"ask multi", "AskUserQuestion_multi", "Question"},
		{"ask single", "AskUserQuestion_single", "Question"},
		{"permission", "PermissionPrompt", "Permission"},
		{"restore", "RestoreCheckpoint", "Restore"},
		{"settings", "Settings", "Settings"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ui := monitor.UIContent{Name: tt.uiName, Content: "Some content"}
			got := formatInteractiveContent(ui)
			if got != "["+tt.wantName+"]\nSome content" {
				t.Errorf("got %q", got)
			}
		})
	}
}

func TestCallbackDataPrefixes(t *testing.T) {
	callbacks := []string{
		"nav_up", "nav_down", "nav_left", "nav_right",
		"nav_space", "nav_tab", "nav_esc", "nav_enter",
		"nav_refresh",
	}
	for _, cb := range callbacks {
		if len(cb) < 4 || cb[:4] != "nav_" {
			t.Errorf("callback %q should start with nav_", cb)
		}
	}
}
