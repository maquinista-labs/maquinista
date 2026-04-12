package bot

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/bridge"
)

// taskPickerState holds state for an active task picker inline keyboard.
type taskPickerState struct {
	Tasks   []bridge.Task
	Mode    string // "pick" or "pickw"
	ChatID  int64
	ThreadID int
	MessageID int
}

// resolveTaskID resolves a (possibly partial) task ID against the project's task list.
// Returns:
//   - (task, true) if exactly one match
//   - (zero, false) + sends picker/error if zero or multiple matches
func (b *Bot) resolveTaskID(msg *tgbotapi.Message, partialID, mode string) (bridge.Task, bool) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	threadIDStr := strconv.Itoa(threadID)

	project, ok := b.state.GetProject(threadIDStr)
	if !ok {
		b.reply(chatID, threadID, "No project bound. Use /p_bind <name> first.")
		return bridge.Task{}, false
	}

	tasks, err := b.minuanoBridge.Status(project)
	if err != nil {
		log.Printf("Error getting tasks for project %s: %v", project, err)
		b.reply(chatID, threadID, "Error: failed to get tasks.")
		return bridge.Task{}, false
	}

	// Filter to actionable tasks (ready or pending)
	var actionable []bridge.Task
	for _, t := range tasks {
		if t.Status == "ready" || t.Status == "pending" {
			actionable = append(actionable, t)
		}
	}

	if partialID == "" {
		// No argument: show all actionable tasks
		b.showTaskPicker(msg, actionable, mode, project)
		return bridge.Task{}, false
	}

	// Try exact match first
	for _, t := range tasks {
		if t.ID == partialID {
			return t, true
		}
	}

	// Try prefix match across all tasks (not just actionable)
	var matches []bridge.Task
	for _, t := range tasks {
		if strings.HasPrefix(t.ID, partialID) {
			matches = append(matches, t)
		}
	}

	switch len(matches) {
	case 0:
		b.reply(chatID, threadID, fmt.Sprintf("No task matching '%s'. Use /t_%s without arguments to see available tasks.", partialID, mode))
		return bridge.Task{}, false
	case 1:
		return matches[0], true
	default:
		// Ambiguous: show matching tasks as picker
		b.showTaskPicker(msg, matches, mode, project)
		return bridge.Task{}, false
	}
}

// showTaskPicker displays an inline keyboard with tasks to choose from.
func (b *Bot) showTaskPicker(msg *tgbotapi.Message, tasks []bridge.Task, mode, project string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	userID := msg.From.ID

	if len(tasks) == 0 {
		b.reply(chatID, threadID, fmt.Sprintf("No ready tasks for project: %s", project))
		return
	}

	// Build inline keyboard: one task per row
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, t := range tasks {
		sym := statusSymbol(t.Status)
		label := fmt.Sprintf("%s %s", sym, truncate(t.Title, 40))
		callbackData := fmt.Sprintf("tpick_%s:%s", mode, t.ID)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, callbackData),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Cancel", "tpick_cancel"),
	))

	kb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	text := fmt.Sprintf("Select a task [%s]:", project)

	sent, err := b.sendMessageWithKeyboard(chatID, threadID, text, kb)
	if err != nil {
		log.Printf("Error sending task picker: %v", err)
		return
	}

	b.mu.Lock()
	b.taskPickerStates[userID] = &taskPickerState{
		Tasks:     tasks,
		Mode:      mode,
		ChatID:    chatID,
		ThreadID:  threadID,
		MessageID: sent.MessageID,
	}
	b.mu.Unlock()
}

// processTaskPickerCallback handles tpick_* callbacks.
func (b *Bot) processTaskPickerCallback(cq *tgbotapi.CallbackQuery) {
	data := cq.Data
	userID := cq.From.ID

	if data == "tpick_cancel" {
		b.mu.Lock()
		tps, ok := b.taskPickerStates[userID]
		if ok {
			delete(b.taskPickerStates, userID)
		}
		b.mu.Unlock()
		if ok {
			b.editMessageText(tps.ChatID, tps.MessageID, "Task selection cancelled.")
		}
		return
	}

	// Parse: tpick_pick:<taskID>, tpick_pickw:<taskID>, or tpick_delete:<taskID>
	var mode, taskID string
	if strings.HasPrefix(data, "tpick_pick:") {
		mode = "pick"
		taskID = data[len("tpick_pick:"):]
	} else if strings.HasPrefix(data, "tpick_pickw:") {
		mode = "pickw"
		taskID = data[len("tpick_pickw:"):]
	} else if strings.HasPrefix(data, "tpick_delete:") {
		mode = "delete"
		taskID = data[len("tpick_delete:"):]
	} else if strings.HasPrefix(data, "tpick_unclaim:") {
		mode = "unclaim"
		taskID = data[len("tpick_unclaim:"):]
	} else {
		log.Printf("Unknown task picker callback: %s", data)
		return
	}

	b.mu.Lock()
	tps, ok := b.taskPickerStates[userID]
	if ok {
		delete(b.taskPickerStates, userID)
	}
	b.mu.Unlock()

	if ok {
		// Find the task title for display
		var title string
		for _, t := range tps.Tasks {
			if t.ID == taskID {
				title = t.Title
				break
			}
		}
		b.editMessageText(tps.ChatID, tps.MessageID, fmt.Sprintf("Selected: %s — %s", taskID, title))
	}

	chatID := cq.Message.Chat.ID
	threadID := getThreadID(cq.Message)

	switch mode {
	case "pick":
		b.executePickTask(chatID, threadID, cq.From.ID, taskID)
	case "pickw":
		b.executePickwTask(chatID, threadID, cq.From.ID, taskID)
	case "delete":
		var title string
		if ok {
			for _, t := range tps.Tasks {
				if t.ID == taskID {
					title = t.Title
					break
				}
			}
		}
		b.executeDeleteTask(chatID, threadID, taskID, title)
	case "unclaim":
		var title string
		if ok {
			for _, t := range tps.Tasks {
				if t.ID == taskID {
					title = t.Title
					break
				}
			}
		}
		b.executeUnclaimTask(chatID, threadID, taskID, title)
	}
}

// executePickTask runs the /pick logic for a resolved task ID.
func (b *Bot) executePickTask(chatID int64, threadID int, userID int64, taskID string) {
	userIDStr := strconv.FormatInt(userID, 10)
	threadIDStr := strconv.Itoa(threadID)

	windowID, bound := b.state.GetWindowForThread(userIDStr, threadIDStr)
	if !bound {
		b.reply(chatID, threadID, "Topic not bound to a session.")
		return
	}

	prompt, err := b.minuanoBridge.PromptSingle(taskID)
	if err != nil {
		log.Printf("Error generating single prompt for %s: %v", taskID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if err := b.sendPromptToTmux(windowID, prompt); err != nil {
		log.Printf("Error sending prompt to tmux: %v", err)
		b.reply(chatID, threadID, "Error: failed to send prompt.")
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Working on task %s...", taskID))
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
