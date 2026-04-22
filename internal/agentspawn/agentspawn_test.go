package agentspawn

import (
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/config"
)

func baseConfig() *config.Config {
	return &config.Config{
		DefaultRunner:   "claude",
		TmuxSessionName: "test",
		MaquinistaBin:   "maquinista",
	}
}

// TestResolveRunnerCmd_NilSoul verifies that hasSoul=false produces no
// --system-prompt flag.
func TestResolveRunnerCmd_NilSoul(t *testing.T) {
	cfg := baseConfig()
	cmd, _ := ResolveRunnerCmd(cfg, "agent-1", "/tmp", false, "")
	if strings.Contains(cmd, "--system-prompt") {
		t.Errorf("expected no --system-prompt, got %q", cmd)
	}
}

// TestResolveRunnerCmd_WithSoul verifies that hasSoul=true for a claude runner
// injects --system-prompt.
func TestResolveRunnerCmd_WithSoul(t *testing.T) {
	cfg := baseConfig()
	cmd, _ := ResolveRunnerCmd(cfg, "agent-1", "/tmp", true, "")
	if !strings.Contains(cmd, "--system-prompt") {
		t.Errorf("expected --system-prompt, got %q", cmd)
	}
}

// TestResolveRunnerCmd_Resume verifies that a non-empty resumeID produces a
// --resume flag and no --system-prompt.
func TestResolveRunnerCmd_Resume(t *testing.T) {
	cfg := baseConfig()
	cmd, _ := ResolveRunnerCmd(cfg, "agent-1", "/tmp", true, "sess-abc123")
	if !strings.Contains(cmd, "--resume") {
		t.Errorf("expected --resume, got %q", cmd)
	}
	if strings.Contains(cmd, "--system-prompt") {
		t.Errorf("expected no --system-prompt with resume, got %q", cmd)
	}
}

// TestSlugifyJobName covers the helper used by dispatchJobSpawn.
func TestSlugifyJobName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"daily digest", "daily-digest"},
		{"Weekly_Report", "weekly-report"},
		{"hello world 123!", "hello-world-123"},
		{"", "job"},
		{strings.Repeat("a", 30), strings.Repeat("a", 20)},
	}
	for _, tt := range tests {
		got := SlugifyJobName(tt.input)
		if got != tt.want {
			t.Errorf("SlugifyJobName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
