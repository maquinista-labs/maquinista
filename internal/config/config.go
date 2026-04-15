package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Telegram settings
	TelegramBotToken    string
	AllowedUsers        []int64
	AllowedGroups       []int64
	QueueTopicID        int64
	ApprovalsTopicID    int64

	// Directories and sessions
	MaquinistaDir        string
	TmuxSessionName string

	// Agent settings
	ClaudeCommand string

	// Monitor
	MonitorPollInterval float64

	// CLI binary path (for bridge)
	MaquinistaBin string

	// Database
	DatabaseURL string

	// Scripts
	ScriptsDir string

	// Project defaults
	DefaultProject    string
	PlannerPromptPath string

	// Default agent runner (claude, opencode, etc.)
	DefaultRunner string

	// DefaultAgent is the agent id auto-spawned by `maquinista start` when
	// no agent with that id is already registered. The spawned tmux window
	// exports AGENT_ID=<DefaultAgent> so the SessionStart hook upserts the
	// agents row. Configured via MAQUINISTA_DEFAULT_AGENT; defaults to
	// "maquinista" if unset. Override on the command line with --agent.
	DefaultAgent string

	// DefaultAgentCWD is the directory the auto-spawned default agent
	// starts in. Configured via MAQUINISTA_DEFAULT_CWD; defaults to the
	// user's home dir when unset. Override with --agent-cwd.
	DefaultAgentCWD string

	// Feature flags (see plans/maquinista-v2-implementation.md §"Feature flags").
	// MailboxOutbound enables shadow-mode writes from the monitor into
	// agent_outbox — the existing Telegram path continues to run so traffic
	// is unaffected. Toggled by MAILBOX_OUTBOUND=1.
	MailboxOutbound bool

	// MailboxInboundTopics is the set of Telegram thread_ids for which
	// inbound user messages route through agent_inbox instead of calling
	// tmux.SendKeysWithDelay directly. Value "*" enables the flag for every
	// topic. Empty list leaves the legacy path untouched.
	// Configured via MAILBOX_INBOUND_TOPICS (comma-separated, or "*").
	MailboxInboundTopics []string

	// MailboxDispatcher flips the Telegram dispatcher from shadow mode (DB
	// rows flow but SendMessage is skipped) to live mode. Shadow mode is
	// the default during the task-1.7 ↔ task-1.8 rollout so channel_deliveries
	// accumulate without double-sending alongside the legacy Telegram path.
	// Configured via MAILBOX_DISPATCHER=1.
	MailboxDispatcher bool
}

// MailboxInboundEnabled reports whether the mailbox.inbound flag is active
// for the given Telegram thread_id. Uses string comparison so callers can
// pass either the numeric thread_id or its string representation consistently
// with the rest of the mailbox code.
func (c *Config) MailboxInboundEnabled(threadID string) bool {
	for _, t := range c.MailboxInboundTopics {
		if t == "*" || t == threadID {
			return true
		}
	}
	return false
}

func Load(envFile ...string) (*Config, error) {
	for _, f := range envFile {
		_ = godotenv.Load(f)
	}
	_ = godotenv.Load() // default .env, ignore if missing

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	usersStr := os.Getenv("ALLOWED_USERS")
	if usersStr == "" {
		return nil, fmt.Errorf("ALLOWED_USERS is required")
	}
	users, err := parseIntList(usersStr)
	if err != nil {
		return nil, fmt.Errorf("invalid ALLOWED_USERS: %w", err)
	}

	var groups []int64
	if g := os.Getenv("ALLOWED_GROUPS"); g != "" {
		groups, err = parseIntList(g)
		if err != nil {
			return nil, fmt.Errorf("invalid ALLOWED_GROUPS: %w", err)
		}
	}

	dir := os.Getenv("MAQUINISTA_DIR")
	if dir == "" {
		dir = "~/.maquinista"
	}
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating maquinista dir: %w", err)
	}

	sessionName := os.Getenv("TMUX_SESSION_NAME")
	if sessionName == "" {
		sessionName = "maquinista"
	}

	claudeCmd := os.Getenv("CLAUDE_COMMAND")
	if claudeCmd == "" {
		claudeCmd = "claude"
	}

	maquinistaBin := os.Getenv("MAQUINISTA_BIN")
	if maquinistaBin == "" {
		maquinistaBin = "maquinista"
	}

	pollInterval := 2.0
	if p := os.Getenv("MONITOR_POLL_INTERVAL"); p != "" {
		pollInterval, err = strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid MONITOR_POLL_INTERVAL: %w", err)
		}
	}

	scriptsDir := os.Getenv("MAQUINISTA_SCRIPTS_DIR")

	var queueTopicID int64
	if q := os.Getenv("MAQUINISTA_QUEUE_TOPIC_ID"); q != "" {
		queueTopicID, _ = strconv.ParseInt(q, 10, 64)
	}

	var approvalsTopicID int64
	if a := os.Getenv("MAQUINISTA_APPROVALS_TOPIC_ID"); a != "" {
		approvalsTopicID, _ = strconv.ParseInt(a, 10, 64)
	}

	defaultProject := os.Getenv("MAQUINISTA_DEFAULT_PROJECT")

	plannerPromptPath := os.Getenv("MAQUINISTA_PLANNER_PROMPT")

	defaultRunner := os.Getenv("MAQUINISTA_DEFAULT_RUNNER")
	if defaultRunner == "" {
		defaultRunner = "claude"
	}

	defaultAgent := strings.TrimSpace(os.Getenv("MAQUINISTA_DEFAULT_AGENT"))
	if defaultAgent == "" {
		defaultAgent = "maquinista"
	}

	defaultAgentCWD := strings.TrimSpace(os.Getenv("MAQUINISTA_DEFAULT_CWD"))
	if defaultAgentCWD != "" {
		defaultAgentCWD = expandHome(defaultAgentCWD)
	}

	return &Config{
		TelegramBotToken:    token,
		AllowedUsers:        users,
		AllowedGroups:       groups,
		MaquinistaDir:            dir,
		TmuxSessionName:     sessionName,
		ClaudeCommand:       claudeCmd,
		MaquinistaBin:            maquinistaBin,
		MonitorPollInterval: pollInterval,
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		ScriptsDir:          scriptsDir,
		QueueTopicID:        queueTopicID,
		ApprovalsTopicID:    approvalsTopicID,
		DefaultProject:      defaultProject,
		PlannerPromptPath:   plannerPromptPath,
		DefaultRunner:       defaultRunner,
		DefaultAgent:        defaultAgent,
		DefaultAgentCWD:     defaultAgentCWD,
		MailboxOutbound:      parseBoolEnv(os.Getenv("MAILBOX_OUTBOUND")),
		MailboxInboundTopics: parseTopicList(os.Getenv("MAILBOX_INBOUND_TOPICS")),
		MailboxDispatcher:    parseBoolEnv(os.Getenv("MAILBOX_DISPATCHER")),
	}, nil
}

func parseTopicList(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (c *Config) IsAllowedUser(userID int64) bool {
	for _, id := range c.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

func (c *Config) IsAllowedGroup(groupID int64) bool {
	if len(c.AllowedGroups) == 0 {
		return true
	}
	for _, id := range c.AllowedGroups {
		if id == groupID {
			return true
		}
	}
	return false
}

func parseIntList(s string) ([]int64, error) {
	var result []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %q: %w", part, err)
		}
		result = append(result, n)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return result, nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
