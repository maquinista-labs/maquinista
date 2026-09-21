package runner

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPiRunner_Registered(t *testing.T) {
	r, err := Get("pi")
	if err != nil {
		t.Fatalf("Get(pi): %v", err)
	}
	if r.Name() != "pi" {
		t.Errorf("Name() = %q", r.Name())
	}
	if _, ok := Runners()["pi"]; !ok {
		t.Error("pi not in Runners()")
	}
}

func TestPiRunner_LaunchCommand(t *testing.T) {
	p := &PiRunner{}
	cmd := p.LaunchCommand(Config{})
	if !strings.HasPrefix(cmd, "pi ") {
		t.Errorf("LaunchCommand = %q, want prefix \"pi \"", cmd)
	}
	if strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Error("pi has no bypass mode; must not appear")
	}
	if strings.Contains(cmd, "OPENCODE_PERMISSION") {
		t.Error("OpenCode-specific env leaked into pi command")
	}
}

func TestPiRunner_InteractiveCommand_EscapesQuotes(t *testing.T) {
	p := &PiRunner{}
	cmd := p.InteractiveCommand(`do "work" now`, Config{})
	if !strings.Contains(cmd, `-p "do \"work\" now"`) {
		t.Errorf("InteractiveCommand did not escape quotes: %s", cmd)
	}
}

func TestPiRunner_EnvOverrides_Empty(t *testing.T) {
	p := &PiRunner{}
	env := p.EnvOverrides()
	if len(env) != 0 {
		t.Errorf("expected empty env overrides, got %v", env)
	}
}

func TestPiRunner_HasSessionHook_False(t *testing.T) {
	if (&PiRunner{}).HasSessionHook() {
		t.Error("pi has no session hook; must be false so the bot writes the preliminary row")
	}
}

func TestPiRunner_PlannerCommand_SystemPrompt(t *testing.T) {
	p := &PiRunner{}
	cmd := p.PlannerCommand("/path/to/system.md", Config{})
	if !strings.Contains(cmd, `--system-prompt "$(cat /path/to/system.md)"`) {
		t.Errorf("PlannerCommand missing --system-prompt file read: %s", cmd)
	}
	// pi has a real --system-prompt; the OpenCode role-framing hack must not appear.
	if strings.Contains(cmd, "SYSTEM INSTRUCTIONS") {
		t.Errorf("persona text must not be inlined: %s", cmd)
	}
}

func TestPiRunner_Model(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("MAQUINISTA_PI_MODEL", "")
		cmd := (&PiRunner{}).LaunchCommand(Config{})
		if !strings.Contains(cmd, `--model "anthropic/claude-sonnet-4-6"`) {
			t.Errorf("default model missing: %s", cmd)
		}
	})
	t.Run("instance wins", func(t *testing.T) {
		t.Setenv("MAQUINISTA_PI_MODEL", "openrouter/qwen/qwen3-coder")
		cmd := (&PiRunner{Model: "openrouter/moonshotai/kimi-k2"}).LaunchCommand(Config{})
		if !strings.Contains(cmd, `"openrouter/moonshotai/kimi-k2"`) {
			t.Errorf("instance Model override ignored: %s", cmd)
		}
		if strings.Contains(cmd, "qwen3-coder") {
			t.Errorf("default leaked after override: %s", cmd)
		}
	})
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("MAQUINISTA_PI_MODEL", "openrouter/qwen/qwen3-coder")
		cmd := (&PiRunner{}).LaunchCommand(Config{})
		if !strings.Contains(cmd, `"openrouter/qwen/qwen3-coder"`) {
			t.Errorf("MAQUINISTA_PI_MODEL ignored: %s", cmd)
		}
	})
}

func TestPiRunner_Provider(t *testing.T) {
	t.Run("bare model gets provider", func(t *testing.T) {
		cmd := (&PiRunner{Model: "qwen3-coder", Provider: "openrouter"}).LaunchCommand(Config{})
		if !strings.Contains(cmd, `--provider "openrouter"`) {
			t.Errorf("bare model missing --provider: %s", cmd)
		}
	})
	t.Run("prefixed model suppresses provider", func(t *testing.T) {
		cmd := (&PiRunner{Model: "openrouter/qwen/qwen3-coder", Provider: "openrouter"}).LaunchCommand(Config{})
		if strings.Contains(cmd, "--provider") {
			t.Errorf("pi rejects --provider with prefixed model: %s", cmd)
		}
	})
}

func TestPiRunner_Thinking(t *testing.T) {
	levels := []string{"off", "minimal", "low", "medium", "high", "xhigh"}
	for _, level := range levels {
		cmd := (&PiRunner{Thinking: level}).LaunchCommand(Config{})
		if !strings.Contains(cmd, `--thinking "`+level+`"`) {
			t.Errorf("level %s missing --thinking: %s", level, cmd)
		}
	}
	// Unset = pi's own default, no flag.
	cmd := (&PiRunner{}).LaunchCommand(Config{})
	if strings.Contains(cmd, "--thinking") {
		t.Errorf("unset thinking must emit no flag: %s", cmd)
	}
}

func TestPiRunner_DetectInstallation(t *testing.T) {
	p := &PiRunner{}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi not on PATH; skipping positive detection")
	}
	if !p.DetectInstallation() {
		t.Error("DetectInstallation() = false, want true (pi on PATH)")
	}
	// Empty PATH → binary can never be found.
	t.Setenv("PATH", "")
	if p.DetectInstallation() {
		t.Error("DetectInstallation() = true with empty PATH")
	}
}
