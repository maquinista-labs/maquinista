package runner

import (
	"context"
	"testing"
)

type mockRunner struct{}

func (m *mockRunner) Name() string                                { return "mock" }
func (m *mockRunner) LaunchCommand(c Config) string                { return "mock" }
func (m *mockRunner) InteractiveCommand(p string, c Config) string { return "mock " + p }
func (m *mockRunner) NonInteractiveArgs(p string, c Config) []string {
	return []string{"mock", "--prompt", p}
}
func (m *mockRunner) RunNonInteractive(ctx context.Context, p string, c Config) (*Result, error) {
	return &Result{ExitCode: 0, Output: "ok"}, nil
}
func (m *mockRunner) PlannerCommand(p string, c Config) string { return "mock --planner " + p }
func (m *mockRunner) DetectInstallation() bool     { return true }
func (m *mockRunner) EnvOverrides() map[string]string { return nil }

func TestRegisterAndGet(t *testing.T) {
	// Clean up after test
	defer func() {
		mu.Lock()
		delete(runners, "mock")
		mu.Unlock()
	}()

	Register("mock", &mockRunner{})

	r, err := Get("mock")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "mock" {
		t.Errorf("Name() = %q, want %q", r.Name(), "mock")
	}
}

func TestGetUnknown(t *testing.T) {
	_, err := Get("nonexistent")
	if err == nil {
		t.Error("expected error for unknown runner")
	}
}

func TestRunners(t *testing.T) {
	defer func() {
		mu.Lock()
		delete(runners, "test")
		mu.Unlock()
	}()

	Register("test", &mockRunner{})
	all := Runners()
	if _, ok := all["test"]; !ok {
		t.Error("expected test runner in Runners()")
	}
}

func TestMockRunnerMethods(t *testing.T) {
	m := &mockRunner{}
	cfg := Config{WorkDir: "/tmp"}

	cmd := m.InteractiveCommand("hello", cfg)
	if cmd != "mock hello" {
		t.Errorf("InteractiveCommand = %q", cmd)
	}

	args := m.NonInteractiveArgs("hello", cfg)
	if len(args) != 3 {
		t.Errorf("NonInteractiveArgs = %v", args)
	}

	res, err := m.RunNonInteractive(context.Background(), "hello", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Output != "ok" {
		t.Errorf("RunNonInteractive = %+v", res)
	}

	if !m.DetectInstallation() {
		t.Error("DetectInstallation = false")
	}

	if m.EnvOverrides() != nil {
		t.Error("EnvOverrides not nil")
	}
}
