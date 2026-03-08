package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
)

// CustomRunner implements AgentRunner for arbitrary binaries.
// Command templates use Go text/template with {{.Prompt}} and {{.Config}} variables.
type CustomRunner struct {
	Binary          string
	InteractiveTpl  string // e.g. "{{.Binary}} run {{.Prompt}}"
	NonInterTpl     string // e.g. "{{.Binary}} --headless -p {{.Prompt}}"
	Env             map[string]string
}

func init() {
	Register("custom", &CustomRunner{
		Binary:         "agent",
		InteractiveTpl: "{{.Binary}} -p {{.Prompt}}",
		NonInterTpl:    "{{.Binary}} --headless -p {{.Prompt}}",
	})
}

type tplData struct {
	Binary string
	Prompt string
}

func (c *CustomRunner) Name() string { return "custom" }

func (c *CustomRunner) LaunchCommand(cfg Config) string {
	return c.Binary
}

func (c *CustomRunner) InteractiveCommand(prompt string, cfg Config) string {
	return c.renderTemplate(c.InteractiveTpl, prompt)
}

func (c *CustomRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
	// Custom runners fall back to interactive command with the system prompt as the prompt.
	return c.InteractiveCommand(fmt.Sprintf("$(cat %s)", systemPromptPath), cfg)
}

func (c *CustomRunner) NonInteractiveArgs(prompt string, cfg Config) []string {
	rendered := c.renderTemplate(c.NonInterTpl, prompt)
	return strings.Fields(rendered)
}

func (c *CustomRunner) RunNonInteractive(ctx context.Context, prompt string, cfg Config) (*Result, error) {
	args := c.NonInteractiveArgs(prompt, cfg)
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
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
			return nil, fmt.Errorf("running custom agent: %w", err)
		}
	}

	return &Result{
		ExitCode: exitCode,
		Output:   stdout.String(),
	}, nil
}

func (c *CustomRunner) DetectInstallation() bool {
	_, err := exec.LookPath(c.Binary)
	return err == nil
}

func (c *CustomRunner) EnvOverrides() map[string]string {
	if c.Env == nil {
		return map[string]string{}
	}
	return c.Env
}

func (c *CustomRunner) renderTemplate(tpl, prompt string) string {
	t, err := template.New("cmd").Parse(tpl)
	if err != nil {
		return fmt.Sprintf("%s %s", c.Binary, prompt)
	}

	var buf bytes.Buffer
	data := tplData{Binary: c.Binary, Prompt: prompt}
	if err := t.Execute(&buf, data); err != nil {
		return fmt.Sprintf("%s %s", c.Binary, prompt)
	}
	return buf.String()
}
