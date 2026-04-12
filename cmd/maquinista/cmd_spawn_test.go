package main

import (
	"testing"
)

func TestSpawnCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "spawn <name>" {
			return
		}
	}
	t.Error("expected 'spawn' command to be registered")
}

func TestSpawnRequiresArg(t *testing.T) {
	rootCmd.SetArgs([]string{"spawn"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected spawn to fail without argument")
	}
}

func TestSpawnCommandFlags(t *testing.T) {
	if spawnCmd.Flags().Lookup("worktrees") == nil {
		t.Error("expected --worktrees flag on spawn command")
	}
}
