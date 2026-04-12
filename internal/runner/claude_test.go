package runner

import (
	"strings"
	"testing"
)

func TestClaudeRunner_Name(t *testing.T) {
	c := &ClaudeRunner{}
	if c.Name() != "claude" {
		t.Errorf("Name() = %q", c.Name())
	}
}

func TestClaudeRunner_InteractiveCommand(t *testing.T) {
	c := &ClaudeRunner{}
	cmd := c.InteractiveCommand("do something", Config{})
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Error("missing --dangerously-skip-permissions")
	}
	if !strings.Contains(cmd, "do something") {
		t.Error("missing prompt")
	}
}

func TestClaudeRunner_EnvOverrides(t *testing.T) {
	c := &ClaudeRunner{}
	env := c.EnvOverrides()
	if _, ok := env["CLAUDECODE"]; !ok {
		t.Error("missing CLAUDECODE env override")
	}
}

func TestClaudeRunner_Registered(t *testing.T) {
	r, err := Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "claude" {
		t.Errorf("registered runner name = %q", r.Name())
	}
}
