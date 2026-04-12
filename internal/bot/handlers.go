package bot

import (
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// handleTextMessage forwards user text to the bound tmux window.
func (b *Bot) handleTextMessage(msg *tgbotapi.Message) {
	userID := strconv.FormatInt(msg.From.ID, 10)
	threadID := strconv.Itoa(getThreadID(msg))
	chatID := msg.Chat.ID

	// Check if this is a reply to an add-task wizard message
	if b.handleAddTaskReply(msg) {
		return
	}

	// Check for pending input (prompt-then-type commands)
	if b.handlePendingInput(msg) {
		return
	}

	// Cancel any running bash capture for this topic
	cancelBashCapture(msg.From.ID, getThreadID(msg))

	// Store group chat ID
	b.state.SetGroupChatID(userID, threadID, chatID)
	b.saveState()

	// Look up window binding
	windowID, bound := b.state.GetWindowForThread(userID, threadID)
	if !bound {
		b.handleUnboundTopic(msg)
		return
	}

	text := msg.Text

	// Handle ! prefix for bash commands
	if strings.HasPrefix(text, "!") && len(text) > 1 {
		b.handleBashCommand(msg, windowID, text)
		return
	}

	// Feature flag mailbox.inbound: route through agent_inbox instead of
	// driving the pty directly. The in-process inboxbridge picks up the row
	// via LISTEN agent_inbox_new and calls SendKeysWithDelay itself, so the
	// pane sees the same keystrokes either way.
	if b.config.MailboxInboundEnabled(threadID) {
		if routed := b.routeTextViaInbox(msg, windowID, text); routed {
			return
		}
		log.Printf("mailbox.inbound: inbox route failed for %s, falling back to legacy path", windowID)
	}

	// Send text to tmux with 500ms delay before Enter
	if err := tmux.SendKeysWithDelay(b.config.TmuxSessionName, windowID, text, 500); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, text)
			return
		}
		log.Printf("Error sending keys to %s: %v", windowID, err)
		b.reply(chatID, getThreadID(msg), "Error: failed to send to Claude session.")
	}
}

// handleUnboundTopic shows window picker or directory browser for an unbound topic.
func (b *Bot) handleUnboundTopic(msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	// Ensure tmux session exists (it may have been destroyed if all windows were closed)
	if err := tmux.EnsureSession(b.config.TmuxSessionName); err != nil {
		log.Printf("Error ensuring tmux session: %v", err)
		b.reply(chatID, threadID, "Error creating tmux session.")
		return
	}

	// Get unbound windows
	windows, err := tmux.ListWindows(b.config.TmuxSessionName)
	if err != nil {
		log.Printf("Error listing windows: %v", err)
		b.reply(chatID, threadID, "Error listing tmux windows.")
		return
	}

	boundWindows := b.state.AllBoundWindowIDs()
	var unboundWindows []tmux.Window
	for _, w := range windows {
		if !boundWindows[w.ID] && w.Name != tmux.InitWindowName {
			unboundWindows = append(unboundWindows, w)
		}
	}

	// Store pending text
	pendingText := msg.Text

	if len(unboundWindows) > 0 {
		b.showWindowPicker(chatID, threadID, userID, unboundWindows, pendingText)
	} else {
		b.showDirectoryBrowser(chatID, threadID, userID, pendingText)
	}
}

// handleBashCommand sends a ! command to Claude's bash mode.
func (b *Bot) handleBashCommand(msg *tgbotapi.Message, windowID, text string) {
	session := b.config.TmuxSessionName

	// Send ! first to enter bash mode
	if err := tmux.SendKeys(session, windowID, "!"); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, text)
			return
		}
		log.Printf("Error sending ! to %s: %v", windowID, err)
		return
	}

	// Wait 1 second
	time.Sleep(1 * time.Second)

	// Send the rest of the command (without !) + Enter
	cmd := text[1:]
	if err := tmux.SendKeysWithDelay(session, windowID, cmd, 500); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, text)
			return
		}
		log.Printf("Error sending bash command to %s: %v", windowID, err)
		return
	}

	// Launch capture goroutine
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	b.startBashCapture(msg.From.ID, chatID, threadID, windowID, cmd)
}

// routeCallback routes callback queries to the appropriate handler.
func (b *Bot) routeCallback(cq *tgbotapi.CallbackQuery) {
	data := cq.Data

	// Answer callback to dismiss spinner
	callback := tgbotapi.NewCallback(cq.ID, "")
	b.api.Request(callback)

	switch {
	case strings.HasPrefix(data, "dir_"):
		b.processDirectoryCallback(cq)
	case strings.HasPrefix(data, "win_"):
		b.processWindowCallback(cq)
	case strings.HasPrefix(data, "hist_"):
		b.handleHistoryCallback(cq)
	case strings.HasPrefix(data, "ss_"):
		b.handleScreenshotCallback(cq)
	case strings.HasPrefix(data, "nav_"):
		b.handleInteractiveCallback(cq)
	case strings.HasPrefix(data, "get_"):
		b.processFileBrowserCallback(cq)
	case strings.HasPrefix(data, "task_"):
		b.processAddTaskCallback(cq)
	case strings.HasPrefix(data, "tpick_"):
		b.processTaskPickerCallback(cq)
	case strings.HasPrefix(data, "merge_"):
		b.handleMergeCallback(cq)
	case strings.HasPrefix(data, "plan_"):
		b.processPlanCallback(cq)
	case strings.HasPrefix(data, "planner_"):
		b.processPlannerCallback(cq, data)
	case strings.HasPrefix(data, "approval_"):
		b.processApprovalCallback(cq)
	case strings.HasPrefix(data, "agent_"):
		b.processAgentCallback(cq, data)
	case strings.HasPrefix(data, "menu_"):
		b.handleMenuCallback(cq)
	case data == "noop":
		// No-op button (e.g., page counter), already answered above
	default:
		log.Printf("Unknown callback data: %s", data)
	}
}

// handleHistoryCallback handles history pagination callbacks.
func (b *Bot) handleHistoryCallback(cq *tgbotapi.CallbackQuery) {
	b.handleHistoryCB(cq)
}

// handleScreenshotCallback handles screenshot control callbacks.
func (b *Bot) handleScreenshotCallback(cq *tgbotapi.CallbackQuery) {
	b.handleScreenshotCB(cq)
}

// handleInteractiveCallback is implemented in interactive.go
