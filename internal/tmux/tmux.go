package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Window represents a tmux window.
type Window struct {
	ID   string // e.g. "@12"
	Name string
	CWD  string
}

// SessionExists checks if a tmux session exists.
func SessionExists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// InitWindowName is the name given to the placeholder window created by EnsureSession.
const InitWindowName = "_init"

// EnsureSession creates a tmux session if it doesn't exist.
// The initial placeholder window is named "_init" so the window picker can skip it.
func EnsureSession(name string) error {
	if SessionExists(name) {
		return nil
	}
	cmd := exec.Command("tmux", "new-session", "-d", "-s", name, "-n", InitWindowName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating session %s: %s: %w", name, string(out), err)
	}
	return nil
}

// ListWindows returns all windows in a session.
func ListWindows(session string) ([]Window, error) {
	cmd := exec.Command("tmux", "list-windows", "-t", session,
		"-F", "#{window_id}\t#{window_name}\t#{pane_current_path}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing windows in %s: %w", session, err)
	}

	var windows []Window
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		windows = append(windows, Window{
			ID:   parts[0],
			Name: parts[1],
			CWD:  parts[2],
		})
	}
	return windows, nil
}

// NewWindow creates a new window, sets env vars, and starts the Claude command.
// Returns the window ID.
func NewWindow(session, name, dir, claudeCmd string, env map[string]string) (string, error) {
	args := []string{"new-window", "-t", session, "-n", name, "-c", dir, "-P", "-F", "#{window_id}"}
	cmd := exec.Command("tmux", args...)
	cmdEnv := os.Environ()
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	cmd.Env = cmdEnv

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("creating window %s in %s: %w", name, session, err)
	}
	windowID := strings.TrimSpace(string(out))

	// Set environment variables inside the tmux window.
	target := session + ":" + windowID
	for k, v := range env {
		expanded := os.ExpandEnv(v)
		setEnvCmd := exec.Command("tmux", "set-environment", "-t", target, k, expanded)
		_ = setEnvCmd.Run()
		setCmd := exec.Command("tmux", "send-keys", "-t", target,
			fmt.Sprintf("export %s=%q", k, expanded), "Enter")
		_ = setCmd.Run()
	}

	// Start Claude (or other agent command)
	if claudeCmd != "" {
		time.Sleep(200 * time.Millisecond)
		startCmd := exec.Command("tmux", "send-keys", "-t", target, claudeCmd, "Enter")
		if err := startCmd.Run(); err != nil {
			return windowID, fmt.Errorf("starting command in %s: %w", windowID, err)
		}
	}

	return windowID, nil
}

// NewWindowWithDir creates a new window starting in the given directory.
// Simpler variant that doesn't start a command or return a window ID.
func NewWindowWithDir(session, name, dir string, env map[string]string) error {
	args := []string{"new-window", "-t", session, "-n", name, "-c", dir}
	cmd := exec.Command("tmux", args...)

	cmdEnv := os.Environ()
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	cmd.Env = cmdEnv

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating window %s:%s in %s: %s: %w", session, name, dir, string(out), err)
	}

	target := session + ":" + name
	for k, v := range env {
		expanded := os.ExpandEnv(v)
		setEnvCmd := exec.Command("tmux", "set-environment", "-t", target, k, expanded)
		_ = setEnvCmd.Run()
		setCmd := exec.Command("tmux", "send-keys", "-t", target,
			fmt.Sprintf("export %s=%q", k, expanded), "Enter")
		_ = setCmd.Run()
	}

	return nil
}

// WindowExists checks if a window exists in the given session.
func WindowExists(session, window string) bool {
	target := session + ":" + window
	return exec.Command("tmux", "select-window", "-t", target).Run() == nil
}

// SendKeys sends literal text to a tmux window (no implicit Enter).
func SendKeys(session, windowID, keys string) error {
	target := session + ":" + windowID
	cmd := exec.Command("tmux", "send-keys", "-t", target, "-l", keys)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("send-keys to %s: %s: %w", target, string(out), err)
	}
	return nil
}

// SendEnter sends the Enter key to a tmux window.
func SendEnter(session, windowID string) error {
	target := session + ":" + windowID
	cmd := exec.Command("tmux", "send-keys", "-t", target, "Enter")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("send-enter to %s: %s: %w", target, string(out), err)
	}
	return nil
}

// SendKeysWithDelay sends text, waits delayMs, then sends Enter.
func SendKeysWithDelay(session, windowID, text string, delayMs int) error {
	if err := SendKeys(session, windowID, text); err != nil {
		return err
	}
	time.Sleep(time.Duration(delayMs) * time.Millisecond)
	return SendEnter(session, windowID)
}

// SendSpecialKey sends a named key (e.g., "Escape", "Up", "Down") to a tmux window.
func SendSpecialKey(session, windowID, key string) error {
	target := session + ":" + windowID
	cmd := exec.Command("tmux", "send-keys", "-t", target, key)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("send-key %s to %s: %s: %w", key, target, string(out), err)
	}
	return nil
}

// CapturePane captures visible pane content.
// If withAnsi is true, includes ANSI escape codes (-e flag) for screenshot rendering.
func CapturePane(session, windowID string, withAnsi bool) (string, error) {
	target := session + ":" + windowID
	args := []string{"capture-pane", "-t", target, "-p"}
	if withAnsi {
		args = append(args, "-e")
	}
	cmd := exec.Command("tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capturing pane %s: %w", target, err)
	}
	return string(out), nil
}

// CapturePaneLines captures the last N lines from a tmux pane.
func CapturePaneLines(session, window string, lines int) (string, error) {
	target := session + ":" + window
	start := fmt.Sprintf("-%d", lines)
	cmd := exec.Command("tmux", "capture-pane", "-t", target, "-p", "-S", start)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capturing pane %s: %w", target, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// IsWindowDead checks if a tmux error indicates the target window/session no longer exists.
func IsWindowDead(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "can't find")
}

// CleanupInitWindow kills the placeholder _init window if it still exists.
func CleanupInitWindow(session string) {
	windows, err := ListWindows(session)
	if err != nil {
		return
	}
	for _, w := range windows {
		if w.Name == InitWindowName {
			_ = KillWindow(session, w.ID)
			return
		}
	}
}

// KillSession kills an entire tmux session and all its windows.
func KillSession(session string) error {
	cmd := exec.Command("tmux", "kill-session", "-t", session)
	if out, err := cmd.CombinedOutput(); err != nil {
		wrapped := fmt.Errorf("killing session %s: %s: %w", session, string(out), err)
		if IsWindowDead(wrapped) {
			return nil // session didn't exist
		}
		return wrapped
	}
	return nil
}

// KillWindow kills a tmux window. Returns nil if window doesn't exist.
func KillWindow(session, windowID string) error {
	target := session + ":" + windowID
	cmd := exec.Command("tmux", "kill-window", "-t", target)
	if out, err := cmd.CombinedOutput(); err != nil {
		wrapped := fmt.Errorf("killing window %s: %s: %w", target, string(out), err)
		if IsWindowDead(wrapped) {
			return nil
		}
		return wrapped
	}
	return nil
}

// WaitForReady polls the pane until Claude Code's TUI chrome separator is visible.
func WaitForReady(session, windowID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text, err := CapturePane(session, windowID, false)
		if err == nil && hasChromeSeparator(text) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func hasChromeSeparator(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		count := 0
		for _, r := range trimmed {
			if r == '─' || r == '━' {
				count++
			}
		}
		if count >= 20 {
			return true
		}
	}
	return false
}

// DisplayMessage runs tmux display-message and returns the output.
func DisplayMessage(paneID, format string) (string, error) {
	cmd := exec.Command("tmux", "display-message", "-t", paneID, "-p", format)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("display-message for %s: %w", paneID, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// GetWindowID returns the tmux internal window ID (e.g. "@5") for a window
// identified by session and name. Used to build session_map keys for runners
// that lack a SessionStart hook.
func GetWindowID(session, windowName string) (string, error) {
	target := session + ":" + windowName
	return DisplayMessage(target, "#{window_id}")
}

// RenameWindow renames a tmux window.
func RenameWindow(session, windowID, newName string) error {
	target := session + ":" + windowID
	cmd := exec.Command("tmux", "rename-window", "-t", target, newName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("renaming window %s: %s: %w", target, string(out), err)
	}
	return nil
}

// AttachSession attaches to a tmux session, optionally selecting a window.
// This replaces the current process.
func AttachSession(session, window string) error {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}

	if window != "" {
		target := session + ":" + window
		exec.Command("tmux", "select-window", "-t", target).Run()
	}

	args := []string{"tmux", "attach-session", "-t", session}
	return syscall.Exec(tmuxBin, args, os.Environ())
}

// SwitchWindow switches to a window when already inside tmux.
func SwitchWindow(session, window string) error {
	target := session + ":" + window
	cmd := exec.Command("tmux", "select-window", "-t", target)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("switching to window %s: %s: %w", target, string(out), err)
	}
	return nil
}

// InsideTmux returns true if the current process is running inside tmux.
func InsideTmux() bool {
	return os.Getenv("TMUX") != ""
}

// AttachOrSwitch attaches to a session/window or switches if already in tmux.
func AttachOrSwitch(session, window string) error {
	if InsideTmux() {
		return SwitchWindow(session, window)
	}
	return AttachSession(session, window)
}
