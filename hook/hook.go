package hook

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/otaviocarvalho/volta/internal/state"
	"github.com/otaviocarvalho/volta/internal/tmux"
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
// Does NOT import config package — uses VOLTA_DIR env or ~/.volta.
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

	// Resolve volta dir
	dir := os.Getenv("VOLTA_DIR")
	if dir == "" {
		dir = "~/.volta"
	}
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating volta dir: %w", err)
	}

	sessionMapPath := filepath.Join(dir, "session_map.json")

	return state.ReadModifyWriteSessionMap(sessionMapPath, func(data map[string]state.SessionMapEntry) {
		data[key] = state.SessionMapEntry{
			SessionID:  input.SessionID,
			CWD:        input.CWD,
			WindowName: windowName,
		}
	})
}

// EnsureInstalled checks if the hook is installed and installs it if not.
// Silent when the hook is already present.
func EnsureInstalled() error {
	return install(false)
}

// Install adds the volta hook to ~/.claude/settings.json.
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

	// Remove any invalid flat-format entries containing "volta hook"
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
			if strings.Contains(cmd, "volta hook") {
				return true
			}
		}
		// Old format: {type, command, timeout} directly
		cmd, _ := m["command"].(string)
		if strings.Contains(cmd, "volta hook") {
			return true
		}
	}
	return false
}
