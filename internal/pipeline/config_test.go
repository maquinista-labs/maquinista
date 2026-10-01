package pipeline

import (
	"testing"
	"time"
)

func TestBridgeConfig_Disabled(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")            // unset
	t.Setenv("MAQUINISTA_LINEAR_TEAM_ID", "") // unset
	t.Setenv("MAQUINISTA_LINEAR_PROJECT", "") // unset
	t.Setenv("MAQUINISTA_PROJECT", "")        // unset
	t.Setenv("MAQUINISTA_LINEAR_POLL", "")    // unset
	if cfg := FromEnv(); cfg.Enabled() {
		t.Error("bridge enabled with no API key and no team")
	}

	t.Setenv("LINEAR_API_KEY", "some-key")
	if cfg := FromEnv(); cfg.Enabled() {
		t.Error("bridge enabled with a key but no team")
	}

	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("MAQUINISTA_LINEAR_TEAM_ID", "team-maq")
	if cfg := FromEnv(); cfg.Enabled() {
		t.Error("bridge enabled with a team but no key")
	}
}

func TestBridgeConfig_Enabled(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "some-key")
	t.Setenv("MAQUINISTA_LINEAR_TEAM_ID", "team-maq")
	t.Setenv("MAQUINISTA_LINEAR_PROJECT", "")
	t.Setenv("MAQUINISTA_PROJECT", "brisa")
	t.Setenv("MAQUINISTA_LINEAR_POLL", "")

	cfg := FromEnv()
	if !cfg.Enabled() {
		t.Fatal("bridge disabled with key + team set")
	}
	if cfg.Interval != 60*time.Second {
		t.Errorf("default interval = %s, want 60s", cfg.Interval)
	}
	if cfg.Project != "brisa" {
		t.Errorf("project fallback = %q, want brisa (MAQUINISTA_PROJECT)", cfg.Project)
	}
	if cfg.TeamID != "team-maq" || cfg.APIKey != "some-key" {
		t.Errorf("team/key not picked up: %q/%q", cfg.TeamID, cfg.APIKey)
	}

	t.Setenv("MAQUINISTA_LINEAR_POLL", "5s")
	if cfg := FromEnv(); cfg.Interval != 5*time.Second {
		t.Errorf("override interval = %s, want 5s", cfg.Interval)
	}

	t.Setenv("MAQUINISTA_LINEAR_POLL", "not-a-duration")
	if cfg := FromEnv(); cfg.Interval != 60*time.Second {
		t.Errorf("invalid override interval = %s, want the 60s default", cfg.Interval)
	}

	t.Setenv("MAQUINISTA_LINEAR_PROJECT", "brisa-prod")
	if cfg := FromEnv(); cfg.Project != "brisa-prod" {
		t.Errorf("explicit project = %q, want brisa-prod to win over the fallback", cfg.Project)
	}
}
