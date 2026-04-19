package bot

import (
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// handleMenuCommand sends an inline keyboard with categorized commands.
func (b *Bot) handleMenuCommand(msg *tgbotapi.Message) {
	kb := buildMenuKeyboard()
	b.sendMessageWithKeyboard(msg.Chat.ID, getThreadID(msg), "Commands:", kb)
}

// buildMenuKeyboard returns the /menu inline keyboard grouped by category.
func buildMenuKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		// Terminal header
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("── Terminal ──", "noop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Screenshot", "menu_c_screenshot"),
			tgbotapi.NewInlineKeyboardButtonData("Esc", "menu_c_esc"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Clear", "menu_c_clear"),
			tgbotapi.NewInlineKeyboardButtonData("Help", "menu_c_help"),
			tgbotapi.NewInlineKeyboardButtonData("Get", "menu_c_get"),
		),
		// Project header
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("── Project ──", "noop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Bind", "menu_p_bind"),
			tgbotapi.NewInlineKeyboardButtonData("Tasks", "menu_p_tasks"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Add", "menu_p_add"),
			tgbotapi.NewInlineKeyboardButtonData("Delete", "menu_p_delete"),
			tgbotapi.NewInlineKeyboardButtonData("History", "menu_p_history"),
		),
		// Task Execution header
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("── Task Execution ──", "noop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Pick", "menu_t_pick"),
			tgbotapi.NewInlineKeyboardButtonData("Pickw", "menu_t_pickw"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Auto", "menu_t_auto"),
			tgbotapi.NewInlineKeyboardButtonData("Batch", "menu_t_batch"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Merge", "menu_t_merge"),
			tgbotapi.NewInlineKeyboardButtonData("Plan", "menu_t_plan"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Unclaim", "menu_t_unclaim"),
		),
		// Tunnel header
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("── Tunnel ──", "noop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Dashboard", "menu_dashboard"),
			tgbotapi.NewInlineKeyboardButtonData("Stop Tunnel", "menu_dashboard_stop"),
		),
	)
}

// handleMenuCallback dispatches a menu button press to the appropriate handler.
func (b *Bot) handleMenuCallback(cq *tgbotapi.CallbackQuery) {
	cmd := strings.TrimPrefix(cq.Data, "menu_")
	msg := syntheticMessage(cq)

	switch cmd {
	case "c_esc":
		b.handleEsc(msg)
	case "c_screenshot":
		b.handleScreenshot(msg)
	case "c_clear":
		b.forwardCommand(msg, "clear")
	case "c_help":
		b.forwardCommand(msg, "help")
	case "c_get":
		b.handleGet(msg)
	case "p_bind":
		b.handleProject(msg)
	case "p_tasks":
		b.handleTasks(msg)
	case "p_add":
		b.handleAdd(msg)
	case "p_delete":
		b.handleDeleteCommand(msg)
	case "p_history":
		b.handleHistory(msg)
	case "t_pick":
		b.handlePick(msg)
	case "t_pickw":
		b.handlePickwCommand(msg)
	case "t_auto":
		b.handleAuto(msg)
	case "t_batch":
		b.handleBatch(msg)
	case "t_merge":
		b.handleMergeCommand(msg)
	case "t_plan":
		b.handlePlanCommand(msg)
	case "t_unclaim":
		b.handleUnclaimCommand(msg)
	case "dashboard":
		b.handleDashboard(msg)
	case "dashboard_stop":
		b.handleDashboardStop(msg)
	}
}

// syntheticMessage creates a message-like object from a callback query,
// preserving user identity and chat/thread context.
func syntheticMessage(cq *tgbotapi.CallbackQuery) *tgbotapi.Message {
	msg := *cq.Message
	msg.From = cq.From
	return &msg
}
