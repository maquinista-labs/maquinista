package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStatusSymbol(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"pending", "○"},
		{"ready", "◎"},
		{"claimed", "●"},
		{"done", "✓"},
		{"failed", "✗"},
		{"unknown", "?"},
		{"", "?"},
	}
	for _, tt := range tests {
		got := statusSymbol(tt.status)
		if got != tt.want {
			t.Errorf("statusSymbol(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestTruncateID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"short-id", "short-id"},
		{"exactly-twenty-chars", "exactly-twenty-chars"},
		{"this-is-a-very-long-task-id-that-exceeds-twenty", "this-is-a-very-long-"},
		{"", ""},
	}
	for _, tt := range tests {
		got := truncateID(tt.input)
		if got != tt.want {
			t.Errorf("truncateID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestStatusCommandRegistered makes sure the top-level `status`
// (post-D.4, the daemon status) is still on rootCmd.
func TestStatusCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "status" {
			return
		}
	}
	t.Error("expected 'status' command to be registered")
}

// TestTasksStatusCommandRegistered: the old task-table moved to
// `maquinista tasks status`.
func TestTasksStatusCommandRegistered(t *testing.T) {
	for _, c := range tasksCmd.Commands() {
		if c.Use == "status" {
			return
		}
	}
	t.Error("expected 'tasks status' command to be registered")
}

func TestTasksStatusCommandFlags(t *testing.T) {
	if tasksStatusCmd.Flags().Lookup("project") == nil {
		t.Error("expected --project flag on tasks status command")
	}
	if tasksStatusCmd.Flags().Lookup("json") == nil {
		t.Error("expected --json flag on tasks status command")
	}
}

// TestFormatDaemonStatusTable_Golden pins the exact rendering of the
// status table. If you change headers or column widths, regenerate
// the golden by running `go test ./cmd/maquinista/ -update-golden`.
func TestFormatDaemonStatusTable_Golden(t *testing.T) {
	rows := []daemonStatusRow{
		{Name: "orchestrator", PID: 12345, Alive: true, Log: "/home/x/.maquinista/logs/orchestrator.log"},
		{Name: "dashboard", PID: 0, Alive: false, Log: "/home/x/.maquinista/logs/dashboard.log"},
	}
	got := formatDaemonStatusTable(rows)

	goldenPath := filepath.Join("testdata", "status_table.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s", goldenPath)
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (re-run with UPDATE_GOLDEN=1)", goldenPath, err)
	}
	if got != string(want) {
		t.Fatalf("table drifted from golden; re-run with UPDATE_GOLDEN=1 if intentional.\n--- got\n%s\n--- want\n%s", got, want)
	}
}
