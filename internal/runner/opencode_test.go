package runner

import (
	"strings"
	"testing"
)

func TestOpenCodeRunner_Name(t *testing.T) {
	o := &OpenCodeRunner{}
	if o.Name() != "opencode" {
		t.Errorf("Name() = %q", o.Name())
	}
}

func TestOpenCodeRunner_InteractiveCommand(t *testing.T) {
	o := &OpenCodeRunner{}
	cmd := o.InteractiveCommand("do work", Config{})
	if !strings.Contains(cmd, "opencode run") {
		t.Error("missing opencode run")
	}
	if !strings.Contains(cmd, "do work") {
		t.Error("missing prompt")
	}
}

func TestOpenCodeRunner_EnvOverrides_SkipPermissions(t *testing.T) {
	o := &OpenCodeRunner{SkipPermissions: true}
	env := o.EnvOverrides()
	if _, ok := env["OPENCODE_PERMISSION"]; !ok {
		t.Error("missing OPENCODE_PERMISSION when SkipPermissions=true")
	}
}

func TestOpenCodeRunner_EnvOverrides_NoSkip(t *testing.T) {
	o := &OpenCodeRunner{}
	env := o.EnvOverrides()
	if len(env) != 0 {
		t.Errorf("expected empty env overrides, got %v", env)
	}
}

func TestOpenCodeRunner_ModelDefault(t *testing.T) {
	o := &OpenCodeRunner{}
	launch := o.LaunchCommand(Config{})
	if !strings.Contains(launch, "--model") {
		t.Errorf("LaunchCommand missing --model: %s", launch)
	}
	if !strings.Contains(launch, "opencode/big-pickle") {
		t.Errorf("LaunchCommand missing default model id: %s", launch)
	}

	// Instance override wins.
	o2 := &OpenCodeRunner{Model: "openrouter/moonshotai/kimi-k2"}
	launch2 := o2.LaunchCommand(Config{})
	if !strings.Contains(launch2, "openrouter/moonshotai/kimi-k2") {
		t.Errorf("instance Model override ignored: %s", launch2)
	}
	// Default must not leak once overridden.
	if strings.Contains(launch2, "claude-sonnet") {
		t.Errorf("default model still present after override: %s", launch2)
	}
}

func TestOpenCodeRunner_PlannerCommand_RoleFraming(t *testing.T) {
	o := &OpenCodeRunner{}
	cmd := o.PlannerCommand("/tmp/sysprompt.md", Config{})
	// The resulting shell command must clearly frame the prompt as
	// system-role instructions, not as a task to complete. Also must use
	// `opencode run` (not the retired `--prompt` flag).
	if !strings.Contains(cmd, "opencode run") {
		t.Error("missing opencode run")
	}
	if strings.Contains(cmd, "--prompt") {
		t.Error("still uses the non-existent --prompt flag")
	}
	if !strings.Contains(cmd, "SYSTEM INSTRUCTIONS") {
		t.Error("missing role-framing header; the model will mistake the prompt for a task")
	}
	if !strings.Contains(cmd, "/tmp/sysprompt.md") {
		t.Error("prompt file path not interpolated")
	}
}

func TestOpenCodeRunner_Registered(t *testing.T) {
	r, err := Get("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "opencode" {
		t.Errorf("registered name = %q", r.Name())
	}
}
