package bot

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// windowPickerState holds per-user window picker state.
type windowPickerState struct {
	Windows     []tmux.Window
	PendingText string
	MessageID   int
	ChatID      int64
	ThreadID    int
}

// showWindowPicker sends the window picker keyboard to the user.
func (b *Bot) showWindowPicker(chatID int64, threadID int, userID int64, windows []tmux.Window, pendingText string) {
	text, keyboard := buildWindowPicker(windows)

	msg, err := b.sendMessageWithKeyboard(chatID, threadID, text, keyboard)
	if err != nil {
		log.Printf("Error sending window picker: %v", err)
		return
	}

	b.mu.Lock()
	b.windowCache[userID] = windows
	b.windowPickerStates[userID] = &windowPickerState{
		Windows:     windows,
		PendingText: pendingText,
		MessageID:   msg.MessageID,
		ChatID:      chatID,
		ThreadID:    threadID,
	}
	b.mu.Unlock()
}

// buildWindowPicker builds the inline keyboard for selecting an unbound window.
func buildWindowPicker(windows []tmux.Window) (string, tgbotapi.InlineKeyboardMarkup) {
	var rows [][]tgbotapi.InlineKeyboardButton

	// Window buttons (2 per row)
	for i := 0; i < len(windows); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		label := fmt.Sprintf("%s (%s)", windows[i].Name, shortenPath(windows[i].CWD))
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			truncateName(label, 30),
			fmt.Sprintf("win_bind:%d", i),
		))
		if i+1 < len(windows) {
			label2 := fmt.Sprintf("%s (%s)", windows[i+1].Name, shortenPath(windows[i+1].CWD))
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(
				truncateName(label2, 30),
				fmt.Sprintf("win_bind:%d", i+1),
			))
		}
		rows = append(rows, row)
	}

	// Action row: New Session | Cancel
	actionRow := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("New Session", "win_new"),
		tgbotapi.NewInlineKeyboardButtonData("Cancel", "win_cancel"),
	}
	rows = append(rows, actionRow)

	text := "Select a window to bind to this topic:"
	return text, tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// processWindowCallback handles window picker callback queries.
func (b *Bot) processWindowCallback(cq *tgbotapi.CallbackQuery) {
	userID := cq.From.ID
	data := cq.Data

	log.Printf("DEBUG: processWindowCallback user=%d data=%q", userID, data)

	b.mu.RLock()
	wps, ok := b.windowPickerStates[userID]
	b.mu.RUnlock()

	if !ok {
		log.Printf("DEBUG: no windowPickerState for user=%d", userID)
		return
	}

	// Verify topic match
	threadID := getThreadID(cq.Message)
	if threadID != wps.ThreadID {
		log.Printf("DEBUG: threadID mismatch: callback=%d picker=%d", threadID, wps.ThreadID)
		return
	}

	switch {
	case strings.HasPrefix(data, "win_bind:"):
		b.handleWinBind(cq, wps, userID)
	case data == "win_new":
		b.handleWinNew(cq, wps, userID)
	case data == "win_cancel":
		b.handleWinCancel(cq, wps, userID)
	}
}

func (b *Bot) handleWinBind(cq *tgbotapi.CallbackQuery, wps *windowPickerState, userID int64) {
	idxStr := strings.TrimPrefix(cq.Data, "win_bind:")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(wps.Windows) {
		return
	}

	window := wps.Windows[idx]
	pendingText := wps.PendingText
	chatID := wps.ChatID
	threadID := wps.ThreadID
	messageID := wps.MessageID

	// Clear picker state
	b.mu.Lock()
	delete(b.windowPickerStates, userID)
	delete(b.windowCache, userID)
	b.mu.Unlock()

	// Bind thread to window
	userIDStr := strconv.FormatInt(userID, 10)
	threadIDStr := strconv.Itoa(threadID)
	b.state.BindThread(userIDStr, threadIDStr, window.ID)
	b.state.SetWindowDisplayName(window.ID, window.Name)
	// Default to claude for manually-bound windows (no runner context available)
	if b.state.GetWindowRunner(window.ID) == "claude" {
		// Only set if not already set (preserves existing runner assignment)
		if r := b.DefaultRunner(); r != nil {
			b.state.SetWindowRunner(window.ID, r.Name())
		}
	}
	b.saveState()

	// Use topic name as display name (fall back to window name)
	displayName := getTopicName(threadID)
	if displayName == "" {
		displayName = window.Name
	}

	// Update picker message
	b.editMessageText(chatID, messageID, fmt.Sprintf("Bound to: %s", displayName))

	// Send pending text
	if pendingText != "" {
		if err := tmux.SendKeysWithDelay(b.config.TmuxSessionName, window.ID, pendingText, 500); err != nil {
			log.Printf("Error sending pending text: %v", err)
		}
	}
}

func (b *Bot) handleWinNew(cq *tgbotapi.CallbackQuery, wps *windowPickerState, userID int64) {
	log.Printf("DEBUG: handleWinNew user=%d chatID=%d threadID=%d", userID, wps.ChatID, wps.ThreadID)
	pendingText := wps.PendingText
	chatID := wps.ChatID
	threadID := wps.ThreadID

	// Clear picker state
	b.mu.Lock()
	delete(b.windowPickerStates, userID)
	delete(b.windowCache, userID)
	b.mu.Unlock()

	// Delete picker message
	b.deleteMessage(chatID, wps.MessageID)

	// Show directory browser
	b.showDirectoryBrowser(chatID, threadID, userID, pendingText)
}

func (b *Bot) handleWinCancel(cq *tgbotapi.CallbackQuery, wps *windowPickerState, userID int64) {
	b.mu.Lock()
	delete(b.windowPickerStates, userID)
	delete(b.windowCache, userID)
	b.mu.Unlock()

	b.editMessageText(wps.ChatID, wps.MessageID, "Cancelled.")
}
