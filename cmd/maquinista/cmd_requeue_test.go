package main

import (
	"testing"
)

func TestRequeueCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "requeue <task-id>" {
			return
		}
	}
	t.Error("expected 'requeue <task-id>' command to be registered")
}

func TestRequeueRequiresArg(t *testing.T) {
	rootCmd.SetArgs([]string{"requeue"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected requeue to fail without a task id argument")
	}
}

func TestRequeueCommandFlags(t *testing.T) {
	if requeueCmd.Flags().Lookup("by") == nil {
		t.Error("expected --by flag on requeue command")
	}
}
