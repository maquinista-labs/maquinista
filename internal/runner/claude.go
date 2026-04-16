package runner

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/maquinista-labs/maquinista/internal/monitor"
)

// ClaudeRunner implements AgentRunner for Claude Code.
type ClaudeRunner struct {
	Model     string
	MaxBudget float64
}

func init() {
	Register("claude", &ClaudeRunner{})
}

func (c *ClaudeRunner) Name() string { return "claude" }

func (c *ClaudeRunner) LaunchCommand(cfg Config) string {
	return "IS_SANDBOX=1 claude --dangerously-skip-permissions"
}

func (c *ClaudeRunner) InteractiveCommand(prompt string, cfg Config) string {
	escaped := strings.ReplaceAll(prompt, "\"", "\\\"")
	return fmt.Sprintf("claude --dangerously-skip-permissions -p \"%s\"", escaped)
}

func (c *ClaudeRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
	return fmt.Sprintf("claude --dangerously-skip-permissions --system-prompt \"$(cat %s)\"", systemPromptPath)
}

func (c *ClaudeRunner) DetectInstallation() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (c *ClaudeRunner) EnvOverrides() map[string]string {
	return map[string]string{"CLAUDECODE": ""}
}

func (c *ClaudeRunner) HasSessionHook() bool { return true }

func (c *ClaudeRunner) MonitorProfile() monitor.MonitorProfile { return monitor.ClaudeProfile() }
