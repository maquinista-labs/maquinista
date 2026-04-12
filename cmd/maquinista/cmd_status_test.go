package main

import (
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

func TestStatusCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "status" {
			return
		}
	}
	t.Error("expected 'status' command to be registered")
}

func TestStatusCommandFlags(t *testing.T) {
	if statusCmd.Flags().Lookup("project") == nil {
		t.Error("expected --project flag on status command")
	}
	if statusCmd.Flags().Lookup("json") == nil {
		t.Error("expected --json flag on status command")
	}
}
