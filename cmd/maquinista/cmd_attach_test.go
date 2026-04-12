package main

import (
	"testing"
)

func TestAttachCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "attach [id]" {
			return
		}
	}
	t.Error("expected 'attach' command to be registered")
}

func TestAttachAcceptsOptionalArg(t *testing.T) {
	// The command accepts 0 or 1 args.
	if err := attachCmd.Args(attachCmd, []string{}); err != nil {
		t.Errorf("attach should accept 0 args: %v", err)
	}
	if err := attachCmd.Args(attachCmd, []string{"agent-1"}); err != nil {
		t.Errorf("attach should accept 1 arg: %v", err)
	}
	if err := attachCmd.Args(attachCmd, []string{"a", "b"}); err == nil {
		t.Error("attach should reject 2 args")
	}
}
