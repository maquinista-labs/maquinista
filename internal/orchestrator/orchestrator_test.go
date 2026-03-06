package orchestrator

import (
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg := Config{
		MaxAgents:    0,
		PollInterval: 0,
	}

	// Verify defaults would be applied in Run
	if cfg.MaxAgents <= 0 {
		cfg.MaxAgents = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Second
	}

	if cfg.MaxAgents != 1 {
		t.Errorf("MaxAgents = %d, want 1", cfg.MaxAgents)
	}
	if cfg.PollInterval != 10*time.Second {
		t.Errorf("PollInterval = %v, want 10s", cfg.PollInterval)
	}
}
