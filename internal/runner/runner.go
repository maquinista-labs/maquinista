package runner

import (
	"context"
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

// Result holds the outcome of a non-interactive agent run.
type Result struct {
	ExitCode int
	Output   string
}

// AgentRunner defines the interface for pluggable agent runners.
type AgentRunner interface {
	// Name returns the runner's identifier (e.g. "claude", "opencode").
	Name() string

	// InteractiveCommand returns the shell command string to start an
	// interactive agent session with the given prompt.
	InteractiveCommand(prompt string, cfg Config) string

	// NonInteractiveArgs returns the command and arguments for a
	// non-interactive (headless) agent run.
	NonInteractiveArgs(prompt string, cfg Config) []string

	// RunNonInteractive executes the agent non-interactively and returns the result.
	RunNonInteractive(ctx context.Context, prompt string, cfg Config) (*Result, error)

	// DetectInstallation checks if the runner's binary is available on the system.
	DetectInstallation() bool

	// EnvOverrides returns environment variables the runner needs set.
	EnvOverrides() map[string]string
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
