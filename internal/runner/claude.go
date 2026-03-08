package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeRunner implements AgentRunner for Claude Code.
type ClaudeRunner struct {
	Model      string
	MaxBudget  float64
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

func (c *ClaudeRunner) NonInteractiveArgs(prompt string, cfg Config) []string {
	args := []string{
		"claude",
		"-p", prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--no-session-persistence",
	}
	model := c.Model
	if model == "" {
		model = "sonnet"
	}
	args = append(args, "--model", model)

	if c.MaxBudget > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", c.MaxBudget))
	}

	return args
}

func (c *ClaudeRunner) RunNonInteractive(ctx context.Context, prompt string, cfg Config) (*Result, error) {
	fullArgs := c.NonInteractiveArgs(prompt, cfg)
	// fullArgs[0] is "claude", rest are arguments
	cmd := exec.CommandContext(ctx, fullArgs[0], fullArgs[1:]...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}

	for k, v := range c.EnvOverrides() {
		cmd.Env = append(cmd.Environ(), k+"="+v)
	}
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Environ(), k+"="+v)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("running claude: %w", err)
		}
	}

	return &Result{
		ExitCode: exitCode,
		Output:   stdout.String(),
	}, nil
}

func (c *ClaudeRunner) DetectInstallation() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (c *ClaudeRunner) EnvOverrides() map[string]string {
	return map[string]string{
		"CLAUDECODE": "",
	}
}
