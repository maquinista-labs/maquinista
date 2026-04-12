package bot

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/bridge"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// handleProjectCommand binds a topic to a Minuano project.
func (b *Bot) handleProjectCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	projectName := strings.TrimSpace(msg.CommandArguments())
	if projectName == "" {
		// Show current binding and prompt for new name
		threadIDStr := strconv.Itoa(threadID)
		if proj, ok := b.state.GetProject(threadIDStr); ok {
			b.reply(chatID, threadID, fmt.Sprintf("Current project: %s\n\nSend a name to bind:", proj))
		} else {
			b.reply(chatID, threadID, "No project bound. Send a name to bind:")
		}
		b.setPendingInput(msg.From.ID, "p_bind", chatID, threadID)
		return
	}

	b.executeProjectBind(msg, projectName)
}

// executeProjectBind binds a project name to the current thread.
func (b *Bot) executeProjectBind(msg *tgbotapi.Message, projectName string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)
	b.state.BindProject(threadIDStr, projectName)
	b.saveState()
	b.reply(chatID, threadID, fmt.Sprintf("Bound to project: %s", projectName))
}

// handleTasksCommand shows tasks for the bound project with clickable pick buttons.
func (b *Bot) handleTasksCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)

	project, ok := b.state.GetProject(threadIDStr)
	if !ok {
		b.reply(chatID, threadID, "No project bound. Use /p_bind <name> first.")
		return
	}

	tasks, err := b.minuanoBridge.Status(project)
	if err != nil {
		log.Printf("Error getting tasks for project %s: %v", project, err)
		b.reply(chatID, threadID, "Error: failed to get tasks.")
		return
	}

	if len(tasks) == 0 {
		b.reply(chatID, threadID, fmt.Sprintf("No tasks for project: %s", project))
		return
	}

	// Build text summary
	var lines []string
	lines = append(lines, fmt.Sprintf("Tasks [%s]:", project))
	for _, t := range tasks {
		sym := statusSymbol(t.Status)
		claimedBy := ""
		if t.ClaimedBy != nil {
			claimedBy = fmt.Sprintf(" (%s)", *t.ClaimedBy)
		}
		lines = append(lines, fmt.Sprintf("  %s %s — %s [%s]%s",
			sym, t.ID, t.Title, t.Status, claimedBy))
	}

	// Add inline keyboard buttons for actionable tasks
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, t := range tasks {
		if t.Status != "ready" && t.Status != "pending" {
			continue
		}
		label := fmt.Sprintf("%s %s", statusSymbol(t.Status), truncate(t.Title, 35))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, "tpick_pick:"+t.ID),
		))
	}

	if len(rows) > 0 {
		kb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
		userID := msg.From.ID
		sent, err := b.sendMessageWithKeyboard(chatID, threadID, strings.Join(lines, "\n"), kb)
		if err != nil {
			log.Printf("Error sending tasks with keyboard: %v", err)
			return
		}
		b.mu.Lock()
		b.taskPickerStates[userID] = &taskPickerState{
			Tasks:     tasks,
			Mode:      "pick",
			ChatID:    chatID,
			ThreadID:  threadID,
			MessageID: sent.MessageID,
		}
		b.mu.Unlock()
	} else {
		b.reply(chatID, threadID, strings.Join(lines, "\n"))
	}
}

// handlePickCommand sends a single-task prompt to Claude.
// Supports: /pick (shows task list), /pick <full-id>, /pick <partial-id>
func (b *Bot) handlePickCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	partialID := strings.TrimSpace(msg.CommandArguments())

	task, ok := b.resolveTaskID(msg, partialID, "pick")
	if !ok {
		return // picker shown or error sent
	}

	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(chatID, threadID, "Topic not bound to a session.")
		return
	}

	prompt, err := b.minuanoBridge.PromptSingle(task.ID)
	if err != nil {
		log.Printf("Error generating single prompt for %s: %v", task.ID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if err := b.sendPromptToTmux(windowID, prompt); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, "")
			return
		}
		log.Printf("Error sending prompt to tmux: %v", err)
		b.reply(chatID, threadID, "Error: failed to send prompt.")
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Working on task %s...", task.ID))
}

// handleAutoCommand sends a loop prompt for autonomous task processing.
func (b *Bot) handleAutoCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)

	project, ok := b.state.GetProject(threadIDStr)
	if !ok {
		b.reply(chatID, threadID, "No project bound. Use /p_bind <name> first.")
		return
	}

	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(chatID, threadID, "Topic not bound to a session.")
		return
	}

	prompt, err := b.minuanoBridge.PromptAuto(project)
	if err != nil {
		log.Printf("Error generating auto prompt for %s: %v", project, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if err := b.sendPromptToTmux(windowID, prompt); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, "")
			return
		}
		log.Printf("Error sending prompt to tmux: %v", err)
		b.reply(chatID, threadID, "Error: failed to send prompt.")
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Starting autonomous mode for project %s...", project))
}

// handleBatchCommand sends a multi-task prompt.
func (b *Bot) handleBatchCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	args := strings.Fields(msg.CommandArguments())
	if len(args) == 0 {
		b.reply(chatID, threadID, "Send the task IDs (space-separated):")
		b.setPendingInput(msg.From.ID, "t_batch", chatID, threadID)
		return
	}

	b.executeBatch(msg, args)
}

// executeBatchWithArgs parses a text string into args and executes batch.
func (b *Bot) executeBatchWithArgs(msg *tgbotapi.Message, text string) {
	args := strings.Fields(strings.TrimSpace(text))
	if len(args) == 0 {
		b.reply(msg.Chat.ID, getThreadID(msg), "No task IDs provided.")
		return
	}
	b.executeBatch(msg, args)
}

// executeBatch sends a multi-task prompt to the bound tmux window.
func (b *Bot) executeBatch(msg *tgbotapi.Message, args []string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(chatID, threadID, "Topic not bound to a session.")
		return
	}

	prompt, err := b.minuanoBridge.PromptBatch(args...)
	if err != nil {
		log.Printf("Error generating batch prompt: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if err := b.sendPromptToTmux(windowID, prompt); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, "")
			return
		}
		log.Printf("Error sending prompt to tmux: %v", err)
		b.reply(chatID, threadID, "Error: failed to send prompt.")
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Working on batch: %s...", strings.Join(args, ", ")))
}

// handleDeleteCommand deletes a Minuano task.
func (b *Bot) handleDeleteCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)

	project, ok := b.state.GetProject(threadIDStr)
	if !ok {
		b.reply(chatID, threadID, "No project bound. Use /p_bind <name> first.")
		return
	}

	partialID := strings.TrimSpace(msg.CommandArguments())
	if partialID == "" {
		// Show task picker for deletion
		tasks, err := b.minuanoBridge.Status(project)
		if err != nil {
			log.Printf("Error getting tasks for project %s: %v", project, err)
			b.reply(chatID, threadID, "Error: failed to get tasks.")
			return
		}
		b.showTaskPicker(msg, tasks, "delete", project)
		return
	}

	task, ok := b.resolveTaskIDAll(msg, partialID, project)
	if !ok {
		return
	}

	b.executeDeleteTask(chatID, threadID, task.ID, task.Title)
}

// resolveTaskIDAll resolves a partial task ID against all tasks (not just actionable).
func (b *Bot) resolveTaskIDAll(msg *tgbotapi.Message, partialID, project string) (bridge.Task, bool) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	tasks, err := b.minuanoBridge.Status(project)
	if err != nil {
		log.Printf("Error getting tasks for project %s: %v", project, err)
		b.reply(chatID, threadID, "Error: failed to get tasks.")
		return bridge.Task{}, false
	}

	// Exact match
	for _, t := range tasks {
		if t.ID == partialID {
			return t, true
		}
	}

	// Prefix match
	var matches []bridge.Task
	for _, t := range tasks {
		if strings.HasPrefix(t.ID, partialID) {
			matches = append(matches, t)
		}
	}

	switch len(matches) {
	case 0:
		b.reply(chatID, threadID, fmt.Sprintf("No task matching '%s'.", partialID))
		return bridge.Task{}, false
	case 1:
		return matches[0], true
	default:
		b.showTaskPicker(msg, matches, "delete", project)
		return bridge.Task{}, false
	}
}

// executeDeleteTask deletes a task by ID and sends confirmation.
func (b *Bot) executeDeleteTask(chatID int64, threadID int, taskID, title string) {
	if err := b.minuanoBridge.Delete(taskID); err != nil {
		log.Printf("Error deleting task %s: %v", taskID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Deleted task: %s — %s", taskID, title))
}

// handleUnclaimCommand releases a claimed task back to ready.
func (b *Bot) handleUnclaimCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)

	project, ok := b.state.GetProject(threadIDStr)
	if !ok {
		b.reply(chatID, threadID, "No project bound. Use /p_bind <name> first.")
		return
	}

	partialID := strings.TrimSpace(msg.CommandArguments())

	tasks, err := b.minuanoBridge.Status(project)
	if err != nil {
		log.Printf("Error getting tasks for project %s: %v", project, err)
		b.reply(chatID, threadID, "Error: failed to get tasks.")
		return
	}

	// Filter to claimed tasks only
	var claimed []bridge.Task
	for _, t := range tasks {
		if t.Status == "claimed" {
			claimed = append(claimed, t)
		}
	}

	if partialID == "" {
		// Show picker with claimed tasks
		if len(claimed) == 0 {
			b.reply(chatID, threadID, "No claimed tasks to unclaim.")
			return
		}
		b.showTaskPicker(msg, claimed, "unclaim", project)
		return
	}

	// Resolve partial ID against claimed tasks
	for _, t := range claimed {
		if t.ID == partialID {
			b.executeUnclaimTask(chatID, threadID, t.ID, t.Title)
			return
		}
	}
	var matches []bridge.Task
	for _, t := range claimed {
		if strings.HasPrefix(t.ID, partialID) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		b.reply(chatID, threadID, fmt.Sprintf("No claimed task matching '%s'.", partialID))
	case 1:
		b.executeUnclaimTask(chatID, threadID, matches[0].ID, matches[0].Title)
	default:
		b.showTaskPicker(msg, matches, "unclaim", project)
	}
}

// executeUnclaimTask unclaims a task by ID and sends confirmation.
func (b *Bot) executeUnclaimTask(chatID int64, threadID int, taskID, title string) {
	if err := b.minuanoBridge.Unclaim(taskID); err != nil {
		log.Printf("Error unclaiming task %s: %v", taskID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Unclaimed: %s — %s", taskID, title))
}

// sendPromptToTmux writes a prompt to a temp file and sends a reference to tmux.
// Long prompts exceed tmux send-keys limits, so we use a temp file.
func (b *Bot) sendPromptToTmux(windowID, prompt string) error {
	// Write prompt to temp file
	tmpFile, err := os.CreateTemp("", "maquinista-task-*.md")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer tmpFile.Close()

	if _, err := tmpFile.WriteString(prompt); err != nil {
		return fmt.Errorf("writing prompt: %w", err)
	}
	tmpFile.Close()

	// Send reference to tmux
	ref := fmt.Sprintf("Please read and follow the instructions in %s", tmpFile.Name())
	return tmux.SendKeysWithDelay(b.config.TmuxSessionName, windowID, ref, 500)
}

// buildMinuanoEnv returns environment variables to set in tmux windows for Minuano
// integration. Returns nil if MINUANO_DB is not configured.
func (b *Bot) buildMinuanoEnv(windowName string) map[string]string {
	if b.config.DatabaseURL == "" {
		return nil
	}

	env := map[string]string{
		"DATABASE_URL": b.config.DatabaseURL,
		"AGENT_ID":     fmt.Sprintf("maquinista-%s", windowName),
	}

	if b.config.ScriptsDir != "" {
		env["PATH"] = fmt.Sprintf("$PATH:%s", b.config.ScriptsDir)
	}

	return env
}

// statusSymbol returns a display symbol for a task status.
func statusSymbol(status string) string {
	switch status {
	case "pending":
		return "○"
	case "ready":
		return "◎"
	case "claimed":
		return "●"
	case "done":
		return "✓"
	case "failed":
		return "✗"
	default:
		return "?"
	}
}
