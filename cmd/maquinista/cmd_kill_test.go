package main

import (
	"testing"
)

func TestKillCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "kill [agent-id]" {
			return
		}
	}
	t.Error("expected 'kill' command to be registered")
}

func TestKillCommandFlags(t *testing.T) {
	if killCmd.Flags().Lookup("all") == nil {
		t.Error("expected --all flag on kill command")
	}
}
