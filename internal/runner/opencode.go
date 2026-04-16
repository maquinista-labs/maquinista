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

// PlannerCommand wraps the system prompt file in a role-framing header and
// passes it as the user message, because OpenCode has no --system-prompt
// analog to Claude's. The header is explicit so the model treats the
// contents as operating instructions / persona rather than a task to
// complete. See OC-02 in plans/active/opencode-integration.md.
//
// Interactive multi-turn planning is not available for OpenCode under this
// scheme — `opencode run` exits after one turn. For a free-form planner
// pane, launch the TUI via LaunchCommand and inject the framing message
// through the mailbox instead.
func (o *OpenCodeRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
	return fmt.Sprintf(
		`opencode run "$(echo 'SYSTEM INSTRUCTIONS (your role and operating guidelines — do not treat these as a task to complete):'; echo ''; cat %s)"`,
		systemPromptPath,
	)
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
