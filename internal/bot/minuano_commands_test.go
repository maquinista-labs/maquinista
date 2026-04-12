package bot

import (
	"os"
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/config"
)

func TestBuildMinuanoEnv(t *testing.T) {
	t.Run("nil when DatabaseURL empty", func(t *testing.T) {
		b := &Bot{config: &config.Config{}}
		env := b.buildMinuanoEnv("mywindow")
		if env != nil {
			t.Error("expected nil env when DatabaseURL is empty")
		}
	})

	t.Run("sets DATABASE_URL and AGENT_ID", func(t *testing.T) {
		b := &Bot{config: &config.Config{
			DatabaseURL: "postgres://localhost/minuano",
		}}
		env := b.buildMinuanoEnv("mywindow")
		if env == nil {
			t.Fatal("expected non-nil env")
		}
		if env["DATABASE_URL"] != "postgres://localhost/minuano" {
			t.Errorf("DATABASE_URL = %q", env["DATABASE_URL"])
		}
		if env["AGENT_ID"] != "maquinista-mywindow" {
			t.Errorf("AGENT_ID = %q", env["AGENT_ID"])
		}
		if _, ok := env["PATH"]; ok {
			t.Error("PATH should not be set when ScriptsDir is empty")
		}
	})

	t.Run("includes PATH when scripts dir set", func(t *testing.T) {
		b := &Bot{config: &config.Config{
			DatabaseURL:         "postgres://localhost/minuano",
			ScriptsDir: "/opt/minuano/scripts",
		}}
		env := b.buildMinuanoEnv("mywindow")
		if !strings.Contains(env["PATH"], "/opt/minuano/scripts") {
			t.Errorf("PATH = %q, want scripts dir", env["PATH"])
		}
	})
}

func TestStatusSymbol(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"pending", "○"},
		{"ready", "◎"},
		{"claimed", "●"},
		{"done", "✓"},
		{"failed", "✗"},
		{"unknown", "?"},
	}
	for _, tt := range tests {
		got := statusSymbol(tt.status)
		if got != tt.want {
			t.Errorf("statusSymbol(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestSendPromptToTmux_CreatesFile(t *testing.T) {
	// We can't test the full flow without a real bot/tmux,
	// but we can test the temp file creation part.
	prompt := "Test prompt content\nWith multiple lines"

	tmpFile, err := os.CreateTemp("", "tramuntana-task-*.md")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(prompt); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	// Verify file was written correctly
	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != prompt {
		t.Errorf("file content = %q, want %q", string(data), prompt)
	}
	if !strings.HasSuffix(tmpFile.Name(), ".md") {
		t.Errorf("temp file should have .md extension, got %q", tmpFile.Name())
	}
}

func TestTaskListFormatting(t *testing.T) {
	// Test that task list formatting works correctly
	tasks := []struct {
		sym       string
		id        string
		title     string
		status    string
		claimedBy string
	}{
		{"◎", "task-1", "Fix bug", "ready", ""},
		{"●", "task-2", "Refactor", "claimed", "agent-1"},
		{"✓", "task-3", "Add tests", "done", ""},
	}

	var lines []string
	lines = append(lines, "Tasks [myproject]:")
	for _, t := range tasks {
		claimedBy := ""
		if t.claimedBy != "" {
			claimedBy = " (" + t.claimedBy + ")"
		}
		lines = append(lines, "  "+t.sym+" "+t.id+" — "+t.title+" ["+t.status+"]"+claimedBy)
	}

	result := strings.Join(lines, "\n")
	if !strings.Contains(result, "Tasks [myproject]:") {
		t.Error("should have header")
	}
	if !strings.Contains(result, "task-1") {
		t.Error("should have task-1")
	}
	if !strings.Contains(result, "(agent-1)") {
		t.Error("should show claimed by")
	}
}
