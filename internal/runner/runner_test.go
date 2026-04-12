package runner

import "testing"

type mockRunner struct{}

func (m *mockRunner) Name() string                                 { return "mock" }
func (m *mockRunner) LaunchCommand(c Config) string                { return "mock" }
func (m *mockRunner) InteractiveCommand(p string, c Config) string { return "mock " + p }
func (m *mockRunner) PlannerCommand(p string, c Config) string     { return "mock --planner " + p }
func (m *mockRunner) DetectInstallation() bool                     { return true }
func (m *mockRunner) EnvOverrides() map[string]string              { return nil }
func (m *mockRunner) HasSessionHook() bool                         { return false }

func TestRegisterAndGet(t *testing.T) {
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
	if _, err := Get("nonexistent"); err == nil {
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

	if cmd := m.InteractiveCommand("hello", cfg); cmd != "mock hello" {
		t.Errorf("InteractiveCommand = %q", cmd)
	}
	if !m.DetectInstallation() {
		t.Error("DetectInstallation = false")
	}
	if m.EnvOverrides() != nil {
		t.Error("EnvOverrides not nil")
	}
}
