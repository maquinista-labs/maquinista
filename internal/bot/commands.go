package bot

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// handleCommand routes slash commands.
func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	// Clear any pending input — user is issuing a new command
	b.clearPendingInput(msg.From.ID)

	switch msg.Command() {
	case "menu":
		b.handleMenuCommand(msg)
	case "c_clear":
		b.forwardCommand(msg, "clear")
	case "c_compact":
		b.forwardCommand(msg, "compact")
	case "c_cost":
		b.forwardCommand(msg, "cost")
	case "c_help":
		b.forwardCommand(msg, "help")
	case "c_memory":
		b.forwardCommand(msg, "memory")
	case "esc", "c_esc":
		b.handleEsc(msg)
	case "c_screenshot":
		b.handleScreenshot(msg)
	case "p_history":
		b.handleHistory(msg)
	case "p_bind":
		b.handleProject(msg)
	case "p_tasks":
		b.handleTasks(msg)
	case "t_pick":
		b.handlePick(msg)
	case "t_auto":
		b.handleAuto(msg)
	case "t_batch":
		b.handleBatch(msg)
	case "p_add":
		b.handleAdd(msg)
	case "c_get":
		b.handleGet(msg)
	case "t_pickw":
		b.handlePickwCommand(msg)
	case "t_merge":
		b.handleMergeCommand(msg)
	case "p_delete":
		b.handleDeleteCommand(msg)
	case "t_unclaim":
		b.handleUnclaimCommand(msg)
	case "t_plan":
		b.handlePlanCommand(msg)
	case "plan":
		b.handlePlannerCommand(msg)
	case "observe":
		b.handleObserveCommand(msg)
	case "unobserve":
		b.handleUnobserveCommand(msg)
	case "watching":
		b.handleWatchingCommand(msg)
	case "agent_list":
		b.handleAgentListCommand(msg)
	case "agent_spawn":
		b.handleAgentSpawnCommand(msg)
	case "agent_kill":
		b.handleAgentKillCommand(msg)
	case "agent_kill_all":
		b.handleAgentKillAllCommand(msg)
	case "runner":
		b.handleRunnerCommand(msg)
	case "agent_default":
		b.handleAgentDefaultCommand(msg)
	case "agent_rename":
		b.handleAgentRenameCommand(msg)
	case "schedule":
		b.handleScheduleCommand(msg)
	case "hook_register":
		b.handleHookRegisterCommand(msg)
	case "hook_enable":
		b.handleHookEnableCommand(msg)
	case "hook_disable":
		b.handleHookDisableCommand(msg)
	case "jobs":
		b.handleJobsCommand(msg)
	case "hooks":
		b.handleHooksCommand(msg)
	case "job_runs":
		b.handleJobRunsCommand(msg)
	default:
		b.reply(msg.Chat.ID, getThreadID(msg), "Unknown command: /"+msg.Command())
	}
}

// resolveWindow returns the window ID for the user's thread, or empty string if unbound.
func (b *Bot) resolveWindow(msg *tgbotapi.Message) (string, bool) {
	userID := strconv.FormatInt(msg.From.ID, 10)
	threadID := strconv.Itoa(getThreadID(msg))
	return b.state.GetWindowForThread(userID, threadID)
}

// forwardCommand sends a command as text to the bound tmux window.
// claudeCmd is the Claude-side command name (e.g. "clear", not "c_clear").
func (b *Bot) forwardCommand(msg *tgbotapi.Message, claudeCmd string) {
	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(msg.Chat.ID, getThreadID(msg), "Topic not bound to a session. Send a message to bind.")
		return
	}

	cmdText := "/" + claudeCmd
	if err := tmux.SendKeysWithDelay(b.config.TmuxSessionName, windowID, cmdText, 500); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, "")
			return
		}
		log.Printf("Error forwarding command %s to %s: %v", cmdText, windowID, err)
		b.reply(msg.Chat.ID, getThreadID(msg), "Error: failed to send command.")
		return
	}

	// Special handling for /clear: reset session monitoring state
	if claudeCmd == "clear" {
		b.resetSessionTracking(windowID)
	}
}

// resetSessionTracking clears session monitor state for a window after /clear.
func (b *Bot) resetSessionTracking(windowID string) {
	// Remove window state's session info so the monitor starts fresh
	// The monitor_state.json offset will be reset when the new JSONL file appears
	if b.monitorState != nil {
		// Find the session key that matches this window
		sessionMapPath := filepath.Join(b.config.MaquinistaDir, "session_map.json")
		sm, err := loadSessionMapForReset(sessionMapPath)
		if err != nil {
			return
		}
		for key := range sm {
			if windowIDFromKey(key) == windowID {
				b.monitorState.RemoveSession(key)
			}
		}
	}
}

// handleEsc sends Escape key to tmux.
func (b *Bot) handleEsc(msg *tgbotapi.Message) {
	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(msg.Chat.ID, getThreadID(msg), "Topic not bound to a session.")
		return
	}

	if err := tmux.SendSpecialKey(b.config.TmuxSessionName, windowID, "Escape"); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, "")
			return
		}
		log.Printf("Error sending Escape to %s: %v", windowID, err)
		b.reply(msg.Chat.ID, getThreadID(msg), "Error: failed to send Escape.")
	}
}

// handleScreenshot captures and sends a terminal screenshot.
func (b *Bot) handleScreenshot(msg *tgbotapi.Message) {
	b.handleScreenshotCommand(msg)
}

// handleHistory shows paginated session history.
func (b *Bot) handleHistory(msg *tgbotapi.Message) {
	b.handleHistoryCommand(msg)
}

// handleProject binds a topic to a Minuano project.
func (b *Bot) handleProject(msg *tgbotapi.Message) {
	b.handleProjectCommand(msg)
}

// handleTasks shows Minuano tasks.
func (b *Bot) handleTasks(msg *tgbotapi.Message) {
	b.handleTasksCommand(msg)
}

// handlePick picks a Minuano task.
func (b *Bot) handlePick(msg *tgbotapi.Message) {
	b.handlePickCommand(msg)
}

// handleAuto auto-claims a Minuano task.
func (b *Bot) handleAuto(msg *tgbotapi.Message) {
	b.handleAutoCommand(msg)
}

// handleBatch runs batch Minuano tasks.
func (b *Bot) handleBatch(msg *tgbotapi.Message) {
	b.handleBatchCommand(msg)
}

// handleGet starts the file browser for sending files via Telegram.
func (b *Bot) handleGet(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	userID := msg.From.ID

	// Try to start from the bound session's CWD
	startPath := ""
	windowID, bound := b.resolveWindow(msg)
	if bound {
		if ws, ok := b.state.GetWindowState(windowID); ok && ws.CWD != "" {
			startPath = ws.CWD
		}
	}

	// Fall back to home directory
	if startPath == "" {
		home, _ := os.UserHomeDir()
		startPath = home
	}

	b.showFileBrowser(chatID, threadID, userID, startPath)
}

// handleAdd starts the add-task wizard.
func (b *Bot) handleAdd(msg *tgbotapi.Message) {
	b.handleAddCommand(msg)
}

// handleTopicClose handles forum topic close events.
// It kills the tmux window and cleans up all related state.
func (b *Bot) handleTopicClose(msg *tgbotapi.Message) {
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)

	// Find all users bound to this thread and clean up each binding
	cleaned := false
	for _, userID := range b.state.AllUserIDs() {
		windowID, bound := b.state.GetWindowForThread(userID, threadIDStr)
		if !bound {
			continue
		}

		cleaned = true

		// Kill tmux window (ignore errors — may already be dead)
		tmux.KillWindow(b.config.TmuxSessionName, windowID)

		// Clean up state
		b.state.UnbindThread(userID, threadIDStr)
		b.state.RemoveWindowState(windowID)
		b.state.RemoveGroupChatID(userID, threadIDStr)

		// Remove monitor state if available
		if b.monitorState != nil {
			sessionMapPath := filepath.Join(b.config.MaquinistaDir, "session_map.json")
			sm, err := loadSessionMapForReset(sessionMapPath)
			if err == nil {
				for key := range sm {
					if windowIDFromKey(key) == windowID {
						b.monitorState.RemoveSession(key)
						// Also remove from session_map.json
						state.RemoveSessionMapEntry(sessionMapPath, key)
					}
				}
			}
		}
	}

	// Remove project binding for this thread
	b.state.RemoveProject(threadIDStr)

	// Clean up worktree if this thread has one
	if wi, ok := b.state.GetWorktreeInfo(threadIDStr); ok {
		if wi.WorktreeDir != "" && !wi.IsMergeTopic {
			if err := git.WorktreeRemove(wi.RepoRoot, wi.WorktreeDir); err != nil {
				log.Printf("Error removing worktree %s: %v", wi.WorktreeDir, err)
			}
			if err := git.DeleteBranch(wi.RepoRoot, wi.Branch); err != nil {
				log.Printf("Error deleting branch %s: %v", wi.Branch, err)
			}
		}
		b.state.RemoveWorktreeInfo(threadIDStr)
		cleaned = true
	}

	if cleaned {
		b.saveState()
		log.Printf("Topic %d closed: cleaned up bindings and killed window", threadID)
	}
}

// SetMonitorState sets the monitor state reference (called by serve command).
func (b *Bot) SetMonitorState(ms *state.MonitorState) {
	b.monitorState = ms
}

// loadSessionMapForReset loads session_map.json for the /clear reset logic.
func loadSessionMapForReset(path string) (map[string]state.SessionMapEntry, error) {
	return state.LoadSessionMap(path)
}

// windowIDFromKey extracts the window ID from a session map key ("session:@N" → "@N").
func windowIDFromKey(key string) string {
	if idx := strings.LastIndex(key, ":"); idx >= 0 {
		return key[idx+1:]
	}
	return key
}
