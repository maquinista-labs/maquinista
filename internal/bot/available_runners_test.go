package bot

import (
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/monitor"
	"github.com/maquinista-labs/maquinista/internal/runner"
)

// fakeRunner satisfies runner.AgentRunner purely to prove the registry —
// not the wire — feeds the available-runners list (C22).
type fakeRunner struct{}

func (f *fakeRunner) Name() string                                              { return "fakeham" }
func (f *fakeRunner) LaunchCommand(runner.Config) string                        { return "fake" }
func (f *fakeRunner) InteractiveCommand(string, runner.Config) string           { return "fake" }
func (f *fakeRunner) PlannerCommand(string, runner.Config) string               { return "fake" }
func (f *fakeRunner) DetectInstallation() bool                                  { return true }
func (f *fakeRunner) EnvOverrides() map[string]string                             { return nil }
func (f *fakeRunner) HasSessionHook() bool                                      { return false }
func (f *fakeRunner) MonitorProfile() monitor.MonitorProfile                    { return monitor.MonitorProfile{} }

func TestAvailableRunners(t *testing.T) {
	// Sanity: the built-ins are present before we inject anything.
	if _, err := runner.Get("claude"); err != nil {
		t.Fatalf("claude missing from registry: %v", err)
	}
	if _, err := runner.Get("pi"); err != nil {
		t.Fatalf("pi missing from registry: %v", err)
	}

	// Unknown arg errors (this is the branch that triggers the reply).
	if _, err := runner.Get("nope"); err == nil {
		t.Fatal("Get(nope) unexpectedly succeeded")
	}

	// Inject a fake runner: the derived text must include it.
	runner.Register("fakeham", &fakeRunner{})
	defer func() { delete(runner.Runners(), "fakeham") }()

	text := unknownRunnerText("nope")
	if !strings.Contains(text, "fakeham") {
		t.Errorf("unknown-runner text not derived from registry: %s", text)
	}
	if !strings.Contains(text, "pi") || !strings.Contains(text, "claude") {
		t.Errorf("built-ins missing from derived text: %s", text)
	}
}
