package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// OpenCodeRunner implements AgentRunner for OpenCode.
type OpenCodeRunner struct {
	Model           string
	SkipPermissions bool
}

func init() {
	Register("opencode", &OpenCodeRunner{})
}

func (o *OpenCodeRunner) Name() string { return "opencode" }

func (o *OpenCodeRunner) LaunchCommand(cfg Config) string {
	return "opencode"
}

func (o *OpenCodeRunner) InteractiveCommand(prompt string, cfg Config) string {
	escaped := strings.ReplaceAll(prompt, "\"", "\\\"")
	return fmt.Sprintf("opencode run \"%s\"", escaped)
}

func (o *OpenCodeRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
	return fmt.Sprintf("opencode run --prompt \"$(cat %s)\"", systemPromptPath)
}

func (o *OpenCodeRunner) NonInteractiveArgs(prompt string, cfg Config) []string {
	args := []string{
		"opencode",
		"run", prompt,
		"--format", "json",
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	return args
}

func (o *OpenCodeRunner) RunNonInteractive(ctx context.Context, prompt string, cfg Config) (*Result, error) {
	fullArgs := o.NonInteractiveArgs(prompt, cfg)
	cmd := exec.CommandContext(ctx, fullArgs[0], fullArgs[1:]...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}

	for k, v := range o.EnvOverrides() {
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
			return nil, fmt.Errorf("running opencode: %w", err)
		}
	}

	return &Result{
		ExitCode: exitCode,
		Output:   stdout.String(),
	}, nil
}

func (o *OpenCodeRunner) DetectInstallation() bool {
	_, err := exec.LookPath("opencode")
	return err == nil
}

func (o *OpenCodeRunner) EnvOverrides() map[string]string {
	env := make(map[string]string)
	if o.SkipPermissions {
		env["OPENCODE_PERMISSION"] = "skip"
	}
	return env
}
