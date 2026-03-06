package main

import (
	"testing"
)

func TestReclaimCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "reclaim" {
			return
		}
	}
	t.Error("expected 'reclaim' command to be registered")
}

func TestReclaimCommandFlags(t *testing.T) {
	if reclaimCmd.Flags().Lookup("minutes") == nil {
		t.Error("expected --minutes flag on reclaim command")
	}
}
