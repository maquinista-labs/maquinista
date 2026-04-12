package bot

import (
	"testing"

	"github.com/maquinista-labs/maquinista/internal/tmux"
)

func TestBuildWindowPicker_SingleWindow(t *testing.T) {
	windows := []tmux.Window{
		{ID: "@1", Name: "project", CWD: "/home/user/code/project"},
	}

	text, kb := buildWindowPicker(windows)

	if text == "" {
		t.Error("text should not be empty")
	}

	// Should have 2 rows: 1 window row + 1 action row
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(kb.InlineKeyboard))
	}

	// First row: 1 window button
	if len(kb.InlineKeyboard[0]) != 1 {
		t.Errorf("expected 1 button in first row, got %d", len(kb.InlineKeyboard[0]))
	}

	// Last row: New Session + Cancel
	lastRow := kb.InlineKeyboard[len(kb.InlineKeyboard)-1]
	if len(lastRow) != 2 {
		t.Fatalf("action row should have 2 buttons, got %d", len(lastRow))
	}
	if *lastRow[0].CallbackData != "win_new" {
		t.Errorf("first action button should be win_new, got %s", *lastRow[0].CallbackData)
	}
	if *lastRow[1].CallbackData != "win_cancel" {
		t.Errorf("second action button should be win_cancel, got %s", *lastRow[1].CallbackData)
	}
}

func TestBuildWindowPicker_MultipleWindows(t *testing.T) {
	windows := []tmux.Window{
		{ID: "@1", Name: "proj1", CWD: "/home/user/proj1"},
		{ID: "@2", Name: "proj2", CWD: "/home/user/proj2"},
		{ID: "@3", Name: "proj3", CWD: "/home/user/proj3"},
	}

	_, kb := buildWindowPicker(windows)

	// 2 window rows (2 per row, 3 windows = 2 rows) + 1 action row = 3 rows
	if len(kb.InlineKeyboard) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(kb.InlineKeyboard))
	}

	// First row: 2 buttons
	if len(kb.InlineKeyboard[0]) != 2 {
		t.Errorf("first row should have 2 buttons, got %d", len(kb.InlineKeyboard[0]))
	}

	// Second row: 1 button
	if len(kb.InlineKeyboard[1]) != 1 {
		t.Errorf("second row should have 1 button, got %d", len(kb.InlineKeyboard[1]))
	}
}

func TestBuildWindowPicker_CallbackData(t *testing.T) {
	windows := []tmux.Window{
		{ID: "@1", Name: "proj1", CWD: "/tmp"},
		{ID: "@2", Name: "proj2", CWD: "/tmp"},
	}

	_, kb := buildWindowPicker(windows)

	// First row should have correct callback data
	if *kb.InlineKeyboard[0][0].CallbackData != "win_bind:0" {
		t.Errorf("first button should be win_bind:0, got %s", *kb.InlineKeyboard[0][0].CallbackData)
	}
	if *kb.InlineKeyboard[0][1].CallbackData != "win_bind:1" {
		t.Errorf("second button should be win_bind:1, got %s", *kb.InlineKeyboard[0][1].CallbackData)
	}
}

func TestBuildWindowPicker_LongNames(t *testing.T) {
	windows := []tmux.Window{
		{ID: "@1", Name: "very-long-project-name-that-exceeds-limit", CWD: "/home/user/very/long/path/to/project"},
	}

	_, kb := buildWindowPicker(windows)

	// Button text should be truncated (rune count, not byte count)
	btn := kb.InlineKeyboard[0][0]
	runeCount := len([]rune(btn.Text))
	if runeCount > 30 {
		t.Errorf("button text too long: %d runes: %s", runeCount, btn.Text)
	}
}
