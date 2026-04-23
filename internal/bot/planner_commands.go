package bot

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// handlePlannerCommand is the entry point for /plan.
func (b *Bot) handlePlannerCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	topicIDStr := strconv.Itoa(threadID)

	subcommand := strings.TrimSpace(msg.CommandArguments())
	parts := strings.Fields(subcommand)

	if len(parts) == 0 {
		b.plannerStart(msg, chatID, threadID, topicIDStr, "")
		return
	}

	switch parts[0] {
	case "reopen":
		b.plannerReopen(msg, chatID, threadID, topicIDStr)
	case "release":
		b.plannerRelease(chatID, threadID, topicIDStr)
	case "stop":
		b.plannerStop(chatID, threadID, topicIDStr)
	case "status":
		b.plannerStatus(chatID, threadID, topicIDStr)
	default:
		// Treat the whole argument as the project flag: /plan <project>
		b.plannerStart(msg, chatID, threadID, topicIDStr, parts[0])
	}
}

// plannerStart creates a new Telegram topic and tmux window for the planner.
func (b *Bot) plannerStart(msg *tgbotapi.Message, chatID int64, threadID int, topicIDStr, project string) {
	if project == "" {
		project = b.config.DefaultProject
	}
	if project == "" {
		project, _ = b.state.GetProject(topicIDStr)
	}
	if project == "" {
		b.reply(chatID, threadID, "No project specified. Use /plan <project> or set TRAMUNTANA_DEFAULT_PROJECT.")
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Creating planner for %s...", project))

	// Create a new Telegram forum topic for the planner
	topicName := fmt.Sprintf("Planner: %s", project)
	newThreadID, err := b.createForumTopic(chatID, topicName)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error creating planner topic: %v", err))
		return
	}

	// Resolve working directory from the current topic's window (if bound)
	dir := b.resolvePlannerDir(msg)

	// Build environment with Minuano vars
	env := b.buildMinuanoEnv(fmt.Sprintf("planner-%s", project))
	if env == nil {
		env = make(map[string]string)
	}
	env["MINUANO_PROJECT"] = project

	// Build planner command using the configured runner
	var plannerCmd string
	if r := b.DefaultRunner(); r != nil {
		plannerCmd = r.PlannerCommand(b.config.PlannerPromptPath, runner.Config{Env: env})
	} else {
		plannerCmd = fmt.Sprintf("claude --dangerously-skip-permissions --system-prompt \"$(cat %s)\"",
			b.config.PlannerPromptPath)
	}

	// Create tmux window with the planner command
	windowID, err := tmux.NewWindow(b.config.TmuxSessionName, topicName, dir, plannerCmd, env)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error creating planner window: %v", err))
		return
	}

	// Clean up _init placeholder if present
	tmux.CleanupInitWindow(b.config.TmuxSessionName)

	// Wait for Claude Code TUI to be ready
	tmux.WaitForReady(b.config.TmuxSessionName, windowID, 15*time.Second)

	// Bind the new topic to the planner window
	userIDStr := strconv.FormatInt(msg.From.ID, 10)
	newThreadIDStr := strconv.Itoa(newThreadID)
	b.state.BindThread(userIDStr, newThreadIDStr, windowID)
	b.state.SetGroupChatID(userIDStr, newThreadIDStr, chatID)
	b.state.BindProject(newThreadIDStr, project)
	b.state.SetWindowDisplayName(windowID, topicName)
	// Track which runner owns this window for transcript source routing
	if r := b.DefaultRunner(); r != nil {
		b.state.SetWindowRunner(windowID, r.Name())
	} else {
		b.state.SetWindowRunner(windowID, "claude")
	}
	b.saveState()

	// Note: we don't call `minuano planner start` here because it creates
	// a duplicate tmux window in the minuano session. The planner runs
	// entirely within Tramuntana's session management.

	b.reply(chatID, newThreadID, "Planner ready. Send your goals and I'll create draft tasks.\nUse /plan release when done to start execution.")
	b.reply(chatID, threadID, fmt.Sprintf("Planner topic created for %s.", project))
}

// resolvePlannerDir returns the working directory for the planner.
// Uses the current topic's window CWD if available, otherwise falls back to home.
func (b *Bot) resolvePlannerDir(msg *tgbotapi.Message) string {
	userID := strconv.FormatInt(msg.From.ID, 10)
	threadID := strconv.Itoa(getThreadID(msg))

	if windowID, bound := b.state.GetWindowForThread(userID, threadID); bound {
		if ws, ok := b.state.GetWindowState(windowID); ok && ws.CWD != "" {
			return ws.CWD
		}
	}

	home, err := filepath.Abs(".")
	if err != nil {
		return "/tmp"
	}
	return home
}

func (b *Bot) plannerReopen(msg *tgbotapi.Message, chatID int64, threadID int, topicIDStr string) {
	project, _ := b.state.GetProject(topicIDStr)
	if project == "" {
		project = b.config.DefaultProject
	}

	// Check if topic already has a bound window — just restart Claude in it
	userID := strconv.FormatInt(msg.From.ID, 10)
	if windowID, bound := b.state.GetWindowForThread(userID, topicIDStr); bound {
		// Window exists, try to restart the agent in it
		var plannerCmd string
		if r := b.DefaultRunner(); r != nil {
			plannerCmd = r.PlannerCommand(b.config.PlannerPromptPath, runner.Config{})
		} else {
			plannerCmd = fmt.Sprintf("claude --dangerously-skip-permissions --system-prompt \"$(cat %s)\"",
				b.config.PlannerPromptPath)
		}
		// Intentionally bypasses the inbox: plannerCmd is a shell command that
		// relaunches the runner binary, not a user turn message.
		if err := tmux.SendKeysWithDelay(b.config.TmuxSessionName, windowID, plannerCmd, 500); err != nil {
			if tmux.IsWindowDead(err) {
				// Window is dead, fall through to create new one
				b.plannerStart(msg, chatID, threadID, topicIDStr, project)
				return
			}
			b.reply(chatID, threadID, fmt.Sprintf("Error reopening planner: %v", err))
			return
		}

		b.reply(chatID, threadID, "Planner session reopened.")
		return
	}

	// No window bound — start fresh
	b.plannerStart(msg, chatID, threadID, topicIDStr, project)
}

func (b *Bot) plannerRelease(chatID int64, threadID int, topicIDStr string) {
	project, ok := b.state.GetProject(topicIDStr)
	if !ok {
		project = b.config.DefaultProject
	}
	if project == "" {
		b.reply(chatID, threadID, "No project bound. Use /p_bind first.")
		return
	}

	out, err := b.minuanoBridge.Run("draft-release", "--all", "--project", project)
	if err != nil {
		log.Printf("draft-release error: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Error releasing tasks: %v", err))
		return
	}

	// Get tree for confirmation
	tree, _ := b.minuanoBridge.Run("tree", "--project", project)
	result := strings.TrimSpace(out)
	if tree != "" {
		result += "\n\n" + strings.TrimSpace(tree)
	}
	b.reply(chatID, threadID, result)
}

func (b *Bot) plannerStop(chatID int64, threadID int, topicIDStr string) {
	out, err := b.minuanoBridge.Run("planner", "stop", "--topic", topicIDStr)
	if err != nil {
		log.Printf("planner stop error: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	_ = out
	b.reply(chatID, threadID, "Planner session stopped. Draft tasks preserved.")
}

func (b *Bot) plannerStatus(chatID int64, threadID int, topicIDStr string) {
	out, err := b.minuanoBridge.Run("planner", "status")
	if err != nil {
		log.Printf("planner status error: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	b.reply(chatID, threadID, strings.TrimSpace(out))
}

// processPlannerCallback handles inline keyboard callbacks from planner crash alerts.
func (b *Bot) processPlannerCallback(cq *tgbotapi.CallbackQuery, data string) {
	parts := strings.SplitN(data, ":", 2)
	if len(parts) < 2 {
		return
	}
	action, topicIDStr := parts[0], parts[1]

	switch action {
	case "planner_reopen":
		out, err := b.minuanoBridge.Run("planner", "reopen", "--topic", topicIDStr)
		if err != nil {
			b.answerCallback(cq.ID, fmt.Sprintf("Error: %v", err))
			return
		}
		_ = out
		b.answerCallback(cq.ID, "Planner session reopened.")
	}
}
