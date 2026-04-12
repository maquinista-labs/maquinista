package runner

import (
	"strings"
	"testing"
)

func TestCustomRunner_Name(t *testing.T) {
	c := &CustomRunner{Binary: "myagent"}
	if c.Name() != "custom" {
		t.Errorf("Name() = %q", c.Name())
	}
}

func TestCustomRunner_InteractiveCommand(t *testing.T) {
	c := &CustomRunner{
		Binary:         "myagent",
		InteractiveTpl: "{{.Binary}} run {{.Prompt}}",
	}
	cmd := c.InteractiveCommand("hello world", Config{})
	if !strings.Contains(cmd, "myagent run hello world") {
		t.Errorf("InteractiveCommand = %q", cmd)
	}
}

func TestCustomRunner_EnvOverrides(t *testing.T) {
	c := &CustomRunner{
		Binary: "myagent",
		Env:    map[string]string{"MY_VAR": "value"},
	}
	env := c.EnvOverrides()
	if env["MY_VAR"] != "value" {
		t.Errorf("MY_VAR = %q", env["MY_VAR"])
	}
}

func TestCustomRunner_EnvOverrides_Nil(t *testing.T) {
	c := &CustomRunner{Binary: "myagent"}
	env := c.EnvOverrides()
	if len(env) != 0 {
		t.Errorf("expected empty env, got %v", env)
	}
}

func TestCustomRunner_Registered(t *testing.T) {
	r, err := Get("custom")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "custom" {
		t.Errorf("registered name = %q", r.Name())
	}
}
