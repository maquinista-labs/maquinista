package main

import (
	"strings"
	"testing"

	"github.com/otaviocarvalho/volta/internal/db"
)

func TestPromptCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "prompt" {
			// Check subcommands.
			subCmds := map[string]bool{"single <task-id>": false, "auto": false, "batch <id1> [id2] ...": false}
			for _, sc := range c.Commands() {
				subCmds[sc.Use] = true
			}
			for name, found := range subCmds {
				if !found {
					t.Errorf("expected subcommand %q under prompt", name)
				}
			}
			return
		}
	}
	t.Error("expected 'prompt' command to be registered")
}

func TestPromptAutoFlags(t *testing.T) {
	if promptAutoCmd.Flags().Lookup("project") == nil {
		t.Error("expected --project flag on prompt auto command")
	}
}

func TestBuildSinglePrompt(t *testing.T) {
	task := &db.Task{
		ID:       "design-auth-a1b",
		Title:    "Design Auth Flow",
		Body:     "Implement OAuth2 authentication",
		Status:   "ready",
		Priority: 7,
	}
	ctxs := []*db.TaskContext{
		{Kind: "inherited", Content: "Previous work on auth module"},
	}

	prompt := buildSinglePrompt(task, ctxs)

	checks := []string{
		"# Task: Design Auth Flow",
		"`design-auth-a1b`",
		"## Specification",
		"Implement OAuth2 authentication",
		"## Context",
		"INHERITED",
		"Previous work on auth module",
		"## Instructions",
		"volta-pick design-auth-a1b",
		"volta-done design-auth-a1b",
		"Do NOT loop",
		"## Environment",
		"AGENT_ID",
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("single prompt missing %q", c)
		}
	}
}

func TestBuildAutoPrompt(t *testing.T) {
	prompt := buildAutoPrompt("auth-system")

	checks := []string{
		"# Auto Mode — Project: auth-system",
		"volta-claim --project auth-system",
		"Stop and return to interactive mode",
		"volta-done",
		"## Rules",
		"## Environment",
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("auto prompt missing %q", c)
		}
	}
}

func TestBuildBatchPrompt(t *testing.T) {
	entries := []taskWithContext{
		{
			task: &db.Task{ID: "task-a", Title: "Task A", Priority: 5, Body: "Do A"},
			ctxs: nil,
		},
		{
			task: &db.Task{ID: "task-b", Title: "Task B", Priority: 3, Body: "Do B"},
			ctxs: nil,
		},
	}

	prompt := buildBatchPrompt(entries)

	checks := []string{
		"# Batch Mode",
		"2 task(s)",
		"## Task 1: Task A",
		"`task-a`",
		"## Task 2: Task B",
		"`task-b`",
		"volta-pick task-a",
		"volta-pick task-b",
		"volta-done task-a",
		"volta-done task-b",
		"return to interactive mode",
		"## Environment",
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("batch prompt missing %q", c)
		}
	}
}

func TestPromptEnvSection(t *testing.T) {
	env := promptEnvSection()
	if !strings.Contains(env, "AGENT_ID") {
		t.Error("env section missing AGENT_ID")
	}
	if !strings.Contains(env, "DATABASE_URL") {
		t.Error("env section missing DATABASE_URL")
	}
	if !strings.Contains(env, "PATH") {
		t.Error("env section missing PATH")
	}
}
