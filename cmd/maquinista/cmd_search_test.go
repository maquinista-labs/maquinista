package main

import (
	"testing"
)

func TestSearchCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "search <query>" {
			return
		}
	}
	t.Error("expected 'search' command to be registered")
}

func TestSearchRequiresArg(t *testing.T) {
	rootCmd.SetArgs([]string{"search"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected search to fail without argument")
	}
}
