package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/otaviocarvalho/volta/internal/db"
)

func TestShowCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "show <id>" {
			return
		}
	}
	t.Error("expected 'show' command to be registered")
}

func TestShowRequiresArg(t *testing.T) {
	rootCmd.SetArgs([]string{"show"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected show to fail without argument")
	}
}

func TestShowCommandFlags(t *testing.T) {
	if showCmd.Flags().Lookup("json") == nil {
		t.Error("expected --json flag on show command")
	}
}

func TestShowOutputStruct(t *testing.T) {
	// Verify ShowOutput can be marshaled.
	out := ShowOutput{
		Task:    &db.Task{ID: "test", Title: "Test", Status: "ready"},
		Context: []*db.TaskContext{},
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal ShowOutput: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"task"`) {
		t.Error("expected 'task' key in JSON output")
	}
	if !strings.Contains(s, `"context"`) {
		t.Error("expected 'context' key in JSON output")
	}
}
