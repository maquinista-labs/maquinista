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

func TestOpenCodeRunner_Registered(t *testing.T) {
	r, err := Get("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "opencode" {
		t.Errorf("registered name = %q", r.Name())
	}
}
