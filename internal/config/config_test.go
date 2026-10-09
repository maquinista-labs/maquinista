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
		"MONITOR_POLL_INTERVAL", "DATABASE_URL", "USER_REPOS",
		"MAQUINISTA_DASHBOARD_LISTEN", "MAQUINISTA_DASHBOARD_AUTH",
		"MAQUINISTA_DASHBOARD_THEME", "MAQUINISTA_DASHBOARD_NODE_BIN",
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

func TestParseUserRepos(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[int64][]string
		err   bool
	}{
		{
			name:  "empty disables bindings",
			input: "",
			want:  nil,
		},
		{
			name:  "single pair",
			input: "111:/srv/repo-a",
			want:  map[int64][]string{111: {"/srv/repo-a"}},
		},
		{
			name:  "repeated user accumulates in order",
			input: "111:/srv/a,222:/srv/b,111:/srv/c",
			want:  map[int64][]string{111: {"/srv/a", "/srv/c"}, 222: {"/srv/b"}},
		},
		{
			name:  "whitespace tolerated, paths cleaned",
			input: " 111 : /srv/a/ , 222:/srv/b/../b ",
			want:  map[int64][]string{111: {"/srv/a"}, 222: {"/srv/b"}},
		},
		{
			name:  "missing colon",
			input: "111-srv-a",
			err:   true,
		},
		{
			name:  "bad user id",
			input: "abc:/srv/a",
			err:   true,
		},
		{
			name:  "empty repo path",
			input: "111:",
			err:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUserRepos(tt.input)
			if tt.err {
				if err == nil {
					t.Fatalf("parseUserRepos(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUserRepos(%q): %v", tt.input, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for id, wantRepos := range tt.want {
				gotRepos, ok := got[id]
				if !ok {
					t.Fatalf("user %d missing from %v", id, got)
				}
				if len(gotRepos) != len(wantRepos) {
					t.Fatalf("user %d repos = %v, want %v", id, gotRepos, wantRepos)
				}
				for i := range wantRepos {
					if gotRepos[i] != wantRepos[i] {
						t.Errorf("user %d repo[%d] = %q, want %q", id, i, gotRepos[i], wantRepos[i])
					}
				}
			}
		})
	}
}

func TestReposFor(t *testing.T) {
	cfg := &Config{UserRepos: map[int64][]string{
		111: {"/srv/a"},
	}}

	repos, restricted := cfg.ReposFor(111)
	if !restricted || len(repos) != 1 || repos[0] != "/srv/a" {
		t.Errorf("ReposFor(111) = %v, %v; want [/srv/a], true", repos, restricted)
	}
	repos, restricted = cfg.ReposFor(999)
	if restricted || repos != nil {
		t.Errorf("ReposFor(999) = %v, %v; want nil, false (unrestricted)", repos, restricted)
	}
}

func TestMayAccessRepo(t *testing.T) {
	cfg := &Config{UserRepos: map[int64][]string{
		111: {"/srv/repo-a"},
	}}

	// Unrestricted user: everything allowed, including unresolvable roots.
	if !cfg.MayAccessRepo(999, "/anywhere") {
		t.Error("unrestricted user denied")
	}
	if !cfg.MayAccessRepo(999, "") {
		t.Error("unrestricted user denied for empty root")
	}

	// Restricted user: exact match after normalization.
	if !cfg.MayAccessRepo(111, "/srv/repo-a") {
		t.Error("bound repo denied")
	}
	if !cfg.MayAccessRepo(111, "/srv/repo-a/") {
		t.Error("trailing slash should normalize to a match")
	}
	if cfg.MayAccessRepo(111, "/srv/repo-b") {
		t.Error("foreign repo allowed")
	}
	// Prefixes/suffixes must not match.
	if cfg.MayAccessRepo(111, "/srv/repo-a-sibling") {
		t.Error("prefix sibling allowed")
	}
	// Fail closed on unresolvable roots.
	if cfg.MayAccessRepo(111, "") {
		t.Error("empty root allowed for restricted user")
	}
}

func TestLoad_UserRepos(t *testing.T) {
	clearEnv()
	tmpDir := t.TempDir()
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "111,222")
	os.Setenv("MAQUINISTA_DIR", tmpDir)
	os.Setenv("USER_REPOS", "111:"+tmpDir+"/repo-a,222:"+tmpDir+"/repo-b,222:"+tmpDir+"/repo-c")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.UserRepos[111]) != 1 || len(cfg.UserRepos[222]) != 2 {
		t.Errorf("UserRepos = %v", cfg.UserRepos)
	}
	if !cfg.MayAccessRepo(222, tmpDir+"/repo-b") || cfg.MayAccessRepo(222, tmpDir+"/repo-a") {
		t.Errorf("MayAccessRepo inconsistent with UserRepos %v", cfg.UserRepos)
	}
}

func TestLoad_InvalidUserRepos(t *testing.T) {
	clearEnv()
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "1")
	os.Setenv("MAQUINISTA_DIR", t.TempDir())
	os.Setenv("USER_REPOS", "not-a-pair")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid USER_REPOS")
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

// --- dashboard config ------------------------------------------------------

func TestLoadDashboard_Defaults(t *testing.T) {
	clearEnv()
	cfg := loadDashboardConfig()
	if cfg.Listen != "127.0.0.1:8900" {
		t.Errorf("Listen = %q, want 127.0.0.1:8900", cfg.Listen)
	}
	if cfg.AuthMode != "password" {
		t.Errorf("AuthMode = %q, want password (secure default)", cfg.AuthMode)
	}
	if cfg.ThemeDefault != "system" {
		t.Errorf("ThemeDefault = %q, want system", cfg.ThemeDefault)
	}
	if cfg.NodeBin != "node" {
		t.Errorf("NodeBin = %q, want node", cfg.NodeBin)
	}
}

func TestLoadDashboard_Overrides(t *testing.T) {
	clearEnv()
	os.Setenv("MAQUINISTA_DASHBOARD_LISTEN", "0.0.0.0:9000")
	os.Setenv("MAQUINISTA_DASHBOARD_AUTH", "password")
	os.Setenv("MAQUINISTA_DASHBOARD_THEME", "dark")
	os.Setenv("MAQUINISTA_DASHBOARD_NODE_BIN", "/opt/node/bin/node")
	cfg := loadDashboardConfig()
	if cfg.Listen != "0.0.0.0:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.AuthMode != "password" {
		t.Errorf("AuthMode = %q", cfg.AuthMode)
	}
	if cfg.ThemeDefault != "dark" {
		t.Errorf("ThemeDefault = %q", cfg.ThemeDefault)
	}
	if cfg.NodeBin != "/opt/node/bin/node" {
		t.Errorf("NodeBin = %q", cfg.NodeBin)
	}
}

func TestLoadDashboard_UnknownAuthFallsBackToPassword(t *testing.T) {
	clearEnv()
	os.Setenv("MAQUINISTA_DASHBOARD_AUTH", "biometric")
	cfg := loadDashboardConfig()
	if cfg.AuthMode != "password" {
		t.Errorf("AuthMode for unknown value = %q, want password (secure fallback)", cfg.AuthMode)
	}
}

func TestLoadDashboard_UnknownThemeFallsBackToSystem(t *testing.T) {
	clearEnv()
	os.Setenv("MAQUINISTA_DASHBOARD_THEME", "neon")
	cfg := loadDashboardConfig()
	if cfg.ThemeDefault != "system" {
		t.Errorf("ThemeDefault for unknown value = %q, want system", cfg.ThemeDefault)
	}
}

func TestLoadDashboard_CaseInsensitive(t *testing.T) {
	clearEnv()
	os.Setenv("MAQUINISTA_DASHBOARD_AUTH", "TELEGRAM")
	os.Setenv("MAQUINISTA_DASHBOARD_THEME", "LIGHT")
	cfg := loadDashboardConfig()
	if cfg.AuthMode != "telegram" {
		t.Errorf("AuthMode lowercased = %q", cfg.AuthMode)
	}
	if cfg.ThemeDefault != "light" {
		t.Errorf("ThemeDefault lowercased = %q", cfg.ThemeDefault)
	}
}

func TestLoad_PopulatesDashboard(t *testing.T) {
	clearEnv()
	tmpDir := t.TempDir()
	os.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	os.Setenv("ALLOWED_USERS", "1")
	os.Setenv("MAQUINISTA_DIR", tmpDir)
	os.Setenv("MAQUINISTA_DASHBOARD_LISTEN", "127.0.0.1:0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dashboard.Listen != "127.0.0.1:0" {
		t.Errorf("cfg.Dashboard.Listen = %q", cfg.Dashboard.Listen)
	}
	if cfg.Dashboard.NodeBin != "node" {
		t.Errorf("cfg.Dashboard.NodeBin = %q", cfg.Dashboard.NodeBin)
	}
}
