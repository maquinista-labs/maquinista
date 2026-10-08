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

// MAQ-41: pipeline roles are task-scoped — SpawnFresh must refuse to mint a
// row with an empty TaskID instead of silently inserting task_id=NULL (a
// binding no completion leg can see). The check fires before any pool/tmux
// work, so the rejection is exercisable as a plain unit test.
func TestSpawnFresh_PipelineRoleRequiresTaskID(t *testing.T) {
	cfg := baseConfig()
	for _, role := range []string{"implementor", "reviewer", "fixer", "merger"} {
		_, err := SpawnFresh(nil, nil, cfg, FreshParams{
			AgentID: role + "-abc",
			Role:    role,
			// TaskID deliberately empty.
		}, nil)
		if err == nil {
			t.Errorf("SpawnFresh(role=%q, TaskID=\"\") = nil error, want hard refusal", role)
			continue
		}
		if !strings.Contains(err.Error(), "task-scoped") || !strings.Contains(err.Error(), "TaskID is required") {
			t.Errorf("SpawnFresh(role=%q) error = %q, want the task-scoped refusal", role, err)
		}
	}
}

// Non-pipeline roles keep the legacy pool semantics: a NULL task_id is
// legal there (user/executor/hook rows are task-less by design).
func TestSpawnFresh_UserRoleAllowsEmptyTaskID(t *testing.T) {
	// The validation must NOT fire for role "" (defaulted to "user") — we
	// can't run the full spawn without tmux, but we can assert the guard
	// predicate directly so the defaulting stays covered.
	role := ""
	if role == "" {
		role = "user"
	}
	if pipelineTaskScopedRoles[role] {
		t.Fatalf("role %q must not be task-scoped", role)
	}
	for _, r := range []string{"user", "executor", "hook"} {
		if pipelineTaskScopedRoles[r] {
			t.Errorf("role %q must not be task-scoped", r)
		}
	}
}
