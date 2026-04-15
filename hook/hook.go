package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// hookInput is the JSON structure read from stdin by the hook.
type hookInput struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
}

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Run executes the SessionStart hook logic:
// reads stdin JSON, gets tmux pane info, writes to session_map.json.
// Does NOT import config package — uses MAQUINISTA_DIR env or ~/.maquinista.
func Run() error {
	var input hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return fmt.Errorf("reading stdin JSON: %w", err)
	}

	if input.HookEventName != "SessionStart" {
		return nil // ignore non-SessionStart hooks
	}

	if !uuidRegex.MatchString(input.SessionID) {
		return fmt.Errorf("invalid session_id: %q", input.SessionID)
	}
	if !filepath.IsAbs(input.CWD) {
		return fmt.Errorf("cwd is not absolute: %q", input.CWD)
	}

	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		return nil // not in tmux, exit silently
	}

	// Get session_name:window_id:window_name from tmux
	info, err := tmux.DisplayMessage(paneID, "#{session_name}:#{window_id}:#{window_name}")
	if err != nil {
		return fmt.Errorf("getting tmux info: %w", err)
	}

	parts := strings.SplitN(info, ":", 3)
	if len(parts) < 3 {
		return fmt.Errorf("unexpected tmux display-message output: %q", info)
	}

	sessionName := parts[0]
	windowID := parts[1]
	windowName := parts[2]
	key := sessionName + ":" + windowID

	// Resolve maquinista dir
	dir := os.Getenv("MAQUINISTA_DIR")
	if dir == "" {
		dir = "~/.maquinista"
	}
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating maquinista dir: %w", err)
	}

	sessionMapPath := filepath.Join(dir, "session_map.json")

	if err := state.ReadModifyWriteSessionMap(sessionMapPath, func(data map[string]state.SessionMapEntry) {
		data[key] = state.SessionMapEntry{
			SessionID:  input.SessionID,
			CWD:        input.CWD,
			WindowName: windowName,
		}
	}); err != nil {
		return err
	}

	// If the user launched the runner with AGENT_ID set, upsert an agents
	// row so the bot's routing ladder can resolve Telegram messages to this
	// session. Fail open — a DB blip here must not crash the Claude session.
	registerAgentFromEnv(sessionName, windowID)
	return nil
}

// registerAgentFromEnv upserts an agents row when AGENT_ID + DATABASE_URL are
// set. Logs and swallows any error.
func registerAgentFromEnv(sessionName, windowID string) {
	agentID := strings.TrimSpace(os.Getenv("AGENT_ID"))
	if agentID == "" {
		return
	}
	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dbURL == "" {
		log.Printf("hook: AGENT_ID=%s set but DATABASE_URL empty; skipping agents upsert", agentID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		log.Printf("hook: agents upsert connect: %v", err)
		return
	}
	defer conn.Close(ctx)

	runner := strings.TrimSpace(os.Getenv("RUNNER_TYPE"))
	if runner == "" {
		runner = "claude"
	}

	if _, err := conn.Exec(ctx, `
		INSERT INTO agents
			(id, tmux_session, tmux_window, role, status, runner_type,
			 started_at, last_seen, stop_requested)
		VALUES ($1, $2, $3, 'user', 'running', $4, NOW(), NOW(), FALSE)
		ON CONFLICT (id) DO UPDATE SET
			tmux_session  = EXCLUDED.tmux_session,
			tmux_window   = EXCLUDED.tmux_window,
			status        = 'running',
			runner_type   = EXCLUDED.runner_type,
			last_seen     = NOW(),
			stop_requested= FALSE
	`, agentID, sessionName, windowID, runner); err != nil {
		log.Printf("hook: agents upsert for %s: %v", agentID, err)
		return
	}
	log.Printf("hook: registered agent %s at %s:%s", agentID, sessionName, windowID)
}

// EnsureInstalled checks if the hook is installed and installs it if not.
// Silent when the hook is already present.
func EnsureInstalled() error {
	return install(false)
}

// Install adds the maquinista hook to ~/.claude/settings.json.
func Install() error {
	return install(true)
}

func install(verbose bool) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting executable path: %w", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")

	// Read existing settings
	var settings map[string]any
	data, err := os.ReadFile(settingsPath)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
			return fmt.Errorf("creating .claude dir: %w", err)
		}
		settings = make(map[string]any)
	} else if err != nil {
		return fmt.Errorf("reading settings: %w", err)
	} else {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parsing settings: %w", err)
		}
	}

	hookCommand := exePath + " hook"

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	sessionStart, _ := hooks["SessionStart"].([]any)

	// Remove any invalid flat-format entries containing "maquinista hook"
	// (these break Claude Code's settings parser and cause all settings to be skipped)
	cleaned := removeInvalidEntries(sessionStart)

	// Check if already installed in valid nested format
	if isHookInstalled(settings, hookCommand) && len(cleaned) == len(sessionStart) {
		if verbose {
			fmt.Println("Hook already installed.")
		}
		return nil
	}

	sessionStart = cleaned

	// Add hook entry if not already present in nested format
	if !isHookInstalled(settings, hookCommand) {
		hookEntry := map[string]any{
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": hookCommand,
					"timeout": 5,
				},
			},
		}
		sessionStart = append(sessionStart, hookEntry)
	}

	hooks["SessionStart"] = sessionStart
	settings["hooks"] = hooks

	// Write back atomically
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}

	log.Println("Installed Claude Code SessionStart hook.")
	return nil
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

// removeInvalidEntries removes SessionStart entries that lack a "hooks" array,
// which would cause Claude Code to reject the entire settings file.
func removeInvalidEntries(entries []any) []any {
	var valid []any
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		if m == nil {
			continue
		}
		if _, ok := m["hooks"]; ok {
			valid = append(valid, entry)
		}
	}
	return valid
}

// isHookInstalled checks if a hook with the given command is already present.
func isHookInstalled(settings map[string]any, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	sessionStart, _ := hooks["SessionStart"].([]any)
	for _, entry := range sessionStart {
		m, _ := entry.(map[string]any)
		if m == nil {
			continue
		}
		// New format: {hooks: [{type, command, timeout}]}
		innerHooks, _ := m["hooks"].([]any)
		for _, h := range innerHooks {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "maquinista hook") {
				return true
			}
		}
		// Old format: {type, command, timeout} directly
		cmd, _ := m["command"].(string)
		if strings.Contains(cmd, "maquinista hook") {
			return true
		}
	}
	return false
}
