package main

import (
	"testing"
)

func TestLogsCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "logs <agent-id>" {
			return
		}
	}
	t.Error("expected 'logs' command to be registered")
}

func TestLogsRequiresArg(t *testing.T) {
	rootCmd.SetArgs([]string{"logs"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected logs to fail without argument")
	}
}

func TestLogsCommandFlags(t *testing.T) {
	if logsCmd.Flags().Lookup("lines") == nil {
		t.Error("expected --lines flag on logs command")
	}
}
