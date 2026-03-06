package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "run" {
			return
		}
	}
	t.Error("expected 'run' command to be registered")
}

func TestRunCommandFlags(t *testing.T) {
	flags := runCmd.Flags()
	expected := []string{"agents", "names", "attach"}
	for _, name := range expected {
		if flags.Lookup(name) == nil {
			t.Errorf("expected flag --%s on run command", name)
		}
	}
}

func TestFindClaudeMD(t *testing.T) {
	// Create temp dir with claude/CLAUDE.md.
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, "claude")
	os.MkdirAll(claudeDir, 0o755)
	os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte("# Agent"), 0o644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(tmp)

	path, err := findClaudeMD()
	if err != nil {
		t.Fatalf("findClaudeMD() error: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
}

func TestFindClaudeMD_NotFound(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(tmp)

	_, err := findClaudeMD()
	if err == nil {
		t.Error("expected error when claude/CLAUDE.md not found")
	}
}
