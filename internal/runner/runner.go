package runner

import (
	"fmt"
	"sync"
)

// Config holds runner-specific configuration passed from the agent/orchestrator.
type Config struct {
	// WorkDir is the working directory for the agent.
	WorkDir string
	// Env is additional environment variables to set.
	Env map[string]string
	// ExtraArgs are runner-specific extra arguments.
	ExtraArgs []string
}

// AgentRunner defines the interface for pluggable agent runners.
//
// α (see plans/reference/maquinista-v2.md §10a) exercises runners exclusively
// through InteractiveCommand — the non-interactive / one-shot surface
// was retired in task 3.7.
type AgentRunner interface {
	// Name returns the runner's identifier (e.g. "claude", "opencode").
	Name() string

	// LaunchCommand returns the shell command to start the interactive TUI
	// without a prompt (for Telegram session binding).
	LaunchCommand(cfg Config) string

	// InteractiveCommand returns the shell command string to start an
	// interactive agent session with the given prompt.
	InteractiveCommand(prompt string, cfg Config) string

	// PlannerCommand returns the shell command string to start an
	// interactive planner session with a system prompt loaded from the given path.
	PlannerCommand(systemPromptPath string, cfg Config) string

	// DetectInstallation checks if the runner's binary is available on the system.
	DetectInstallation() bool

	// EnvOverrides returns environment variables the runner needs set.
	EnvOverrides() map[string]string

	// HasSessionHook returns true if this runner writes session_map entries
	// via an external hook (e.g. Claude Code's SessionStart hook).
	// When false, the bot writes a preliminary session_map entry and the
	// TranscriptSource discovers the session ID later.
	HasSessionHook() bool
}

var (
	mu      sync.RWMutex
	runners = make(map[string]AgentRunner)
)

// Register adds an AgentRunner to the global registry.
func Register(name string, r AgentRunner) {
	mu.Lock()
	defer mu.Unlock()
	runners[name] = r
}

// Get returns a registered AgentRunner by name.
func Get(name string) (AgentRunner, error) {
	mu.RLock()
	defer mu.RUnlock()
	r, ok := runners[name]
	if !ok {
		return nil, fmt.Errorf("unknown runner: %q", name)
	}
	return r, nil
}

// Runners returns a copy of all registered runners.
func Runners() map[string]AgentRunner {
	mu.RLock()
	defer mu.RUnlock()
	copy := make(map[string]AgentRunner, len(runners))
	for k, v := range runners {
		copy[k] = v
	}
	return copy
}
