package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv() {
	for _, key := range []string{
		"TELEGRAM_BOT_TOKEN", "ALLOWED_USERS", "ALLOWED_GROUPS",
		"MAQUINISTA_DIR", "TMUX_SESSION_NAME", "CLAUDE_COMMAND",
		"MONITOR_POLL_INTERVAL", "DATABASE_URL",
	} {
		os.Unsetenv(key)
	}
}

func TestLoad_RequiresToken(t *testing.T) {
	clearEnv()
	os.Setenv("ALLOWED_USERS", "123")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestLoad_RequiresAllowedUsers(t *testing.T) {
	clearEnv()
	os.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing allowed users")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv()
	tmpDir := t.TempDir()
	os.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	os.Setenv("ALLOWED_USERS", "123,456")
	os.Setenv("MAQUINISTA_DIR", tmpDir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.TelegramBotToken != "test-token" {
		t.Errorf("token = %q, want %q", cfg.TelegramBotToken, "test-token")
	}
	if len(cfg.AllowedUsers) != 2 || cfg.AllowedUsers[0] != 123 || cfg.AllowedUsers[1] != 456 {
		t.Errorf("users = %v, want [123, 456]", cfg.AllowedUsers)
	}
	if cfg.TmuxSessionName != "maquinista" {
		t.Errorf("session = %q, want %q", cfg.TmuxSessionName, "maquinista")
	}
	if cfg.ClaudeCommand != "claude" {
		t.Errorf("claude command = %q, want %q", cfg.ClaudeCommand, "claude")
	}
	if cfg.MonitorPollInterval != 2.0 {
		t.Errorf("poll interval = %f, want 2.0", cfg.MonitorPollInterval)
	}
}

func TestLoad_AllowedGroups(t *testing.T) {
	clearEnv()
	tmpDir := t.TempDir()
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "1")
	os.Setenv("ALLOWED_GROUPS", "-100123,-100456")
	os.Setenv("MAQUINISTA_DIR", tmpDir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.AllowedGroups) != 2 {
		t.Errorf("groups = %v, want 2 entries", cfg.AllowedGroups)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	clearEnv()
	tmpDir := t.TempDir()
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "1")
	os.Setenv("MAQUINISTA_DIR", tmpDir)
	os.Setenv("TMUX_SESSION_NAME", "mysess")
	os.Setenv("CLAUDE_COMMAND", "/usr/bin/claude")
	os.Setenv("MONITOR_POLL_INTERVAL", "5.0")
	os.Setenv("DATABASE_URL", "postgres://localhost/maquinista")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TmuxSessionName != "mysess" {
		t.Errorf("session = %q", cfg.TmuxSessionName)
	}
	if cfg.ClaudeCommand != "/usr/bin/claude" {
		t.Errorf("claude = %q", cfg.ClaudeCommand)
	}
	if cfg.MonitorPollInterval != 5.0 {
		t.Errorf("interval = %f", cfg.MonitorPollInterval)
	}
	if cfg.DatabaseURL != "postgres://localhost/maquinista" {
		t.Errorf("db = %q", cfg.DatabaseURL)
	}
}

func TestLoad_CreatesMaquinistaDir(t *testing.T) {
	clearEnv()
	tmpDir := filepath.Join(t.TempDir(), "subdir")
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "1")
	os.Setenv("MAQUINISTA_DIR", tmpDir)

	_, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("maquinista dir was not created")
	}
}

func TestLoad_InvalidPollInterval(t *testing.T) {
	clearEnv()
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "1")
	os.Setenv("MAQUINISTA_DIR", t.TempDir())
	os.Setenv("MONITOR_POLL_INTERVAL", "notanumber")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid poll interval")
	}
}

func TestIsAllowedUser(t *testing.T) {
	cfg := &Config{AllowedUsers: []int64{100, 200, 300}}

	if !cfg.IsAllowedUser(100) {
		t.Error("100 should be allowed")
	}
	if cfg.IsAllowedUser(999) {
		t.Error("999 should not be allowed")
	}
}

func TestIsAllowedGroup(t *testing.T) {
	cfg := &Config{}
	if !cfg.IsAllowedGroup(-100123) {
		t.Error("empty groups should allow all")
	}

	cfg.AllowedGroups = []int64{-100123, -100456}
	if !cfg.IsAllowedGroup(-100123) {
		t.Error("-100123 should be allowed")
	}
	if cfg.IsAllowedGroup(-100999) {
		t.Error("-100999 should not be allowed")
	}
}

func TestParseIntList(t *testing.T) {
	tests := []struct {
		input string
		want  []int64
		err   bool
	}{
		{"1,2,3", []int64{1, 2, 3}, false},
		{" 1 , 2 ", []int64{1, 2}, false},
		{"-100", []int64{-100}, false},
		{"", nil, true},
		{"abc", nil, true},
	}

	for _, tt := range tests {
		got, err := parseIntList(tt.input)
		if tt.err && err == nil {
			t.Errorf("parseIntList(%q) expected error", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("parseIntList(%q) unexpected error: %v", tt.input, err)
		}
		if !tt.err && len(got) != len(tt.want) {
			t.Errorf("parseIntList(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	got := expandHome("~/test")
	want := filepath.Join(home, "test")
	if got != want {
		t.Errorf("expandHome(~/test) = %q, want %q", got, want)
	}

	got = expandHome("/absolute/path")
	if got != "/absolute/path" {
		t.Errorf("expandHome(/absolute/path) = %q", got)
	}
}

func TestLoad_FromEnvFile(t *testing.T) {
	clearEnv()
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	os.WriteFile(envFile, []byte("TELEGRAM_BOT_TOKEN=file-token\nALLOWED_USERS=42\nMAQUINISTA_DIR="+tmpDir+"\n"), 0644)

	cfg, err := Load(envFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TelegramBotToken != "file-token" {
		t.Errorf("token = %q, want file-token", cfg.TelegramBotToken)
	}
}
