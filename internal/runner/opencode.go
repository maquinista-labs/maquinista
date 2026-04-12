package runner

import (
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

func (o *OpenCodeRunner) HasSessionHook() bool { return false }
