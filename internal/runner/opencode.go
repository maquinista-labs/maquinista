package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/maquinista-labs/maquinista/internal/monitor"
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

// defaultOpenCodeModel is the fallback model when neither the runner
// instance nor the OPENCODE_MODEL env supplies one. Set to OpenCode's
// own free model so fresh installs work without provider credentials.
// Operators who want a different provider set OPENCODE_MODEL or pass
// --model on the runner instance. Per OC-05 in
// plans/active/opencode-integration.md.
const defaultOpenCodeModel = "opencode/big-pickle"

// modelFlag returns a `--model <id>` arg when a model is resolved.
// Empty string when neither the runner nor env provides one — OpenCode
// then falls back to its own config default (no surprise for existing
// users, but fresh installs still land on a sensible model via the
// constant above).
func (o *OpenCodeRunner) modelFlag() string {
	model := strings.TrimSpace(o.Model)
	if model == "" {
		if env := strings.TrimSpace(os.Getenv("OPENCODE_MODEL")); env != "" {
			model = env
		}
	}
	if model == "" {
		model = defaultOpenCodeModel
	}
	return fmt.Sprintf("--model %q", model)
}

func (o *OpenCodeRunner) LaunchCommand(cfg Config) string {
	return fmt.Sprintf("opencode %s", o.modelFlag())
}

func (o *OpenCodeRunner) InteractiveCommand(prompt string, cfg Config) string {
	escaped := strings.ReplaceAll(prompt, "\"", "\\\"")
	return fmt.Sprintf("opencode run %s \"%s\"", o.modelFlag(), escaped)
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
		`opencode run %s "$(echo 'SYSTEM INSTRUCTIONS (your role and operating guidelines — do not treat these as a task to complete):'; echo ''; cat %s)"`,
		o.modelFlag(),
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

func (o *OpenCodeRunner) MonitorProfile() monitor.MonitorProfile { return monitor.OpenCodeProfile() }
