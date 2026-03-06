package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainsHealthy(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"(healthy)", true},
		{"Up 2 minutes (healthy)", true},
		{"Up 3 seconds", true},
		{"starting", false},
		{"", false},
	}
	for _, tt := range tests {
		got := containsHealthy(tt.input)
		if got != tt.want {
			t.Errorf("containsHealthy(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFindComposePath(t *testing.T) {
	// Create a temp dir with the expected structure.
	tmp := t.TempDir()
	dockerDir := filepath.Join(tmp, "docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerDir, "docker-compose.yml"), []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Change to temp dir so findComposePath finds it.
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(tmp)

	path, err := findComposePath()
	if err != nil {
		t.Fatalf("findComposePath() error: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
}

func TestFindComposePath_NotFound(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(tmp)

	_, err := findComposePath()
	if err == nil {
		t.Error("expected error when docker-compose.yml not found")
	}
}

func TestComposeCommand(t *testing.T) {
	cmd, args := composeCommand("/path/to/docker-compose.yml", "up", "-d")
	// Should return either "docker" or "docker-compose" depending on the system.
	if cmd != "docker" && cmd != "docker-compose" {
		t.Errorf("composeCommand returned unexpected command: %q", cmd)
	}
	// Args should include -f and the compose path.
	found := false
	for _, a := range args {
		if a == "/path/to/docker-compose.yml" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected compose path in args, got %v", args)
	}
}

func TestUpDownCommandsRegistered(t *testing.T) {
	cmds := rootCmd.Commands()
	foundUp := false
	foundDown := false
	for _, c := range cmds {
		switch c.Use {
		case "up":
			foundUp = true
		case "down":
			foundDown = true
		}
	}
	if !foundUp {
		t.Error("expected 'up' command to be registered")
	}
	if !foundDown {
		t.Error("expected 'down' command to be registered")
	}
}
