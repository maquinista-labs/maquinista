package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/maquinista-labs/maquinista/internal/monitor"
)

// PiRunner implements AgentRunner for the pi coding agent CLI
// (badlogic/pi-mono, command surface verified live against v0.73.1).
//
// pi is one-shot friendly (-p / --print), has a real --system-prompt flag
// (no Claude-style role-framing hack needed), and accepts --provider /
// --model / --thinking. It deliberately has no permission-bypass mode —
// the sandbox is the operator's duty (see README "Runner: pi").
type PiRunner struct {
	Model    string // provider-prefixed ("openrouter/qwen/qwen3-coder") or bare
	Provider string // only meaningful with a bare model id
	Thinking string // off, minimal, low, medium, high, xhigh
}

func init() {
	Register("pi", &PiRunner{})
}

func (p *PiRunner) Name() string { return "pi" }

// defaultPiModel is the fallback model when neither the runner instance
// nor MAQUINISTA_PI_MODEL supplies one — provider-prefixed so fresh
// installs work without a separate pi provider config.
const defaultPiModel = "anthropic/claude-sonnet-4-6"

// resolvedModel: instance > MAQUINISTA_PI_MODEL > default. Note we never
// read bare PI_MODEL here — pi injects PI_MODEL / PI_PROVIDER /
// PI_REASONING_LEVEL / PI_SESSION_FILE into its own bash-tool children as
// *outputs* (docs/environment-variables.md, verified 0.73.1), so honoring
// them would feed the runner its own runtime state.
func (p *PiRunner) resolvedModel() string {
	model := strings.TrimSpace(p.Model)
	if model == "" {
		if env := strings.TrimSpace(os.Getenv("MAQUINISTA_PI_MODEL")); env != "" {
			model = env
		}
	}
	if model == "" {
		model = defaultPiModel
	}
	return model
}

// modelArgs returns the --model argument plus --provider when the model
// id is bare. A provider-prefixed model (contains "/") carries its own
// provider, and pi v0.73.1 rejects the combination — so --provider is
// suppressed in that case.
func (p *PiRunner) modelArgs() string {
	model := p.resolvedModel()
	args := fmt.Sprintf("--model %q", model)
	if !strings.Contains(model, "/") {
		provider := strings.TrimSpace(p.Provider)
		if provider == "" {
			provider = strings.TrimSpace(os.Getenv("MAQUINISTA_PI_PROVIDER"))
		}
		if provider != "" {
			args += fmt.Sprintf(" --provider %q", provider)
		}
	}
	return args
}

// thinkingArgs returns the --thinking argument when a level resolves
// (instance > MAQUINISTA_PI_THINKING). Empty = pi's own default (medium).
func (p *PiRunner) thinkingArgs() string {
	level := strings.TrimSpace(p.Thinking)
	if level == "" {
		level = strings.TrimSpace(os.Getenv("MAQUINISTA_PI_THINKING"))
	}
	if level == "" {
		return ""
	}
	return fmt.Sprintf(" --thinking %q", level)
}

func (p *PiRunner) LaunchCommand(cfg Config) string {
	return fmt.Sprintf("pi %s%s", p.modelArgs(), p.thinkingArgs())
}

func (p *PiRunner) InteractiveCommand(prompt string, cfg Config) string {
	escaped := strings.ReplaceAll(prompt, "\"", "\\\"")
	return fmt.Sprintf("pi %s%s -p \"%s\"", p.modelArgs(), p.thinkingArgs(), escaped)
}

// PlannerCommand uses pi's real --system-prompt flag, reading the persona
// file at launch via shell substitution — the persona text is never
// inlined into the command string.
func (p *PiRunner) PlannerCommand(systemPromptPath string, cfg Config) string {
	return fmt.Sprintf(
		`pi %s%s --system-prompt "$(cat %s)"`,
		p.modelArgs(),
		p.thinkingArgs(),
		systemPromptPath,
	)
}

func (p *PiRunner) DetectInstallation() bool {
	_, err := exec.LookPath("pi")
	return err == nil
}

// EnvOverrides returns nothing: pi has no permission-bypass mode to
// enable and no env we would ever set on its behalf. The API key env
// (PI_KEY or provider-specific vars like OPENROUTER_API_KEY) is the
// operator's shell environment, not ours to inject.
func (p *PiRunner) EnvOverrides() map[string]string { return map[string]string{} }

// HasSessionHook is false: pi has no SessionStart-style hook. The bot
// writes a preliminary session_map row and PiSource backfills agents
// .session_id by scanning pi's session store (same hookless path as
// OpenCode).
func (p *PiRunner) HasSessionHook() bool { return false }

// MonitorProfile: PI-00 probe findings (pi v0.73.1, live observation).
// The TUI draws '─' (U+2500) chrome separator lines like Claude plus a
// bottom bar (cwd + context meter + "model • thinking"). No input-area
// spinner was observed across idle/busy/compaction//login//tree panes,
// and no interactive UI (permission/plan) screens were captured. The
// profile therefore ships empty — nil runes/patterns make every helper
// "never match", so pi panes flow through unparsed instead of being
// misclassified (exactly how OpenCodeProfile started; see OC-06).
func (p *PiRunner) MonitorProfile() monitor.MonitorProfile { return monitor.PiProfile() }
