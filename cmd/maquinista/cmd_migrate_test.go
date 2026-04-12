package main

import (
	"os"
	"testing"
)

func TestMigrateCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "migrate" {
			return
		}
	}
	t.Error("expected 'migrate' command to be registered")
}

func TestMigrateRequiresDB(t *testing.T) {
	// Ensure no DB is available.
	dbURL = ""
	os.Unsetenv("DATABASE_URL")

	rootCmd.SetArgs([]string{"migrate"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected migrate to fail without DATABASE_URL")
	}
}
