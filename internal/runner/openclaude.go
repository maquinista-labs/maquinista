package runner

import (
	"fmt"
	"os/exec"
	"strings"
)

// OpenClaudeRunner implements AgentRunner for OpenClaude.
type OpenClaudeRunner struct {
	Model     string
	MaxBudget float64
}

func init() {
	Register("openclaude", &OpenClaudeRunner{})
}

func (o *OpenClaudeRunner) Name() string { return "openclaude" }

func (o *OpenClaudeRunner) LaunchCommand(cfg Config) string {
	return "IS_SANDBOX=1 openclaude --dangerously-skip-permissions"
}

func (o *OpenClaudeRunner) InteractiveCommand(prompt string, cfg Config) string {
	escaped := strings.ReplaceAll(prompt, "\"", "\\\"")
	return fmt.Sprintf("openclaude --dangerously-skip-permissions -p \"%s\"", escaped)
}

func (o *OpenClaudeRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
	return fmt.Sprintf("openclaude --dangerously-skip-permissions --system-prompt \"$(cat %s)\"", systemPromptPath)
}

func (o *OpenClaudeRunner) DetectInstallation() bool {
	_, err := exec.LookPath("openclaude")
	return err == nil
}

func (o *OpenClaudeRunner) EnvOverrides() map[string]string {
	return map[string]string{"CLAUDECODE": ""}
}

func (o *OpenClaudeRunner) HasSessionHook() bool { return true }
