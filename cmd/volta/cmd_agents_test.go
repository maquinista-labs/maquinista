package main

import (
	"testing"
	"time"
)

func TestRelativeTime(t *testing.T) {
	tests := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"seconds", 30 * time.Second, "30s ago"},
		{"minutes", 5 * time.Minute, "5m ago"},
		{"hours", 2 * time.Hour, "2h ago"},
		{"just now", 0, "0s ago"},
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

func TestAgentsCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "agents" {
			return
		}
	}
	t.Error("expected 'agents' command to be registered")
}

func TestAgentsCommandFlags(t *testing.T) {
	if agentsCmd.Flags().Lookup("watch") == nil {
		t.Error("expected --watch flag on agents command")
	}
}
