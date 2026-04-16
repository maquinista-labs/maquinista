package runner

import (
	"bytes"
	"fmt"
	"os/exec"
	"text/template"

	"github.com/maquinista-labs/maquinista/internal/monitor"
)

// CustomRunner implements AgentRunner for arbitrary binaries.
// Command templates use Go text/template with {{.Prompt}} and {{.Config}} variables.
type CustomRunner struct {
	Binary         string
	InteractiveTpl string // e.g. "{{.Binary}} run {{.Prompt}}"
	Env            map[string]string
}

func init() {
	Register("custom", &CustomRunner{
		Binary:         "agent",
		InteractiveTpl: "{{.Binary}} -p {{.Prompt}}",
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

func (c *CustomRunner) HasSessionHook() bool {
	return false
}

// MonitorProfile returns an empty profile: custom runners have no known
// chrome layout. Override by wrapping with a subtype that supplies one.
func (c *CustomRunner) MonitorProfile() monitor.MonitorProfile { return monitor.MonitorProfile{} }

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
