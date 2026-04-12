package bot

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/monitor"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

var interactiveRetryRe = regexp.MustCompile(`retry after (\d+)`)

// interactiveKey identifies an interactive UI session.
type interactiveKey struct {
	UserID   int64
	ThreadID int
}

// interactiveState tracks interactive UI per user+thread.
type interactiveState struct {
	mu       sync.RWMutex
	messages map[interactiveKey]int    // message_id
	modes    map[interactiveKey]string // window_id
}

var interactive = &interactiveState{
	messages: make(map[interactiveKey]int),
	modes:    make(map[interactiveKey]string),
}

// handleInteractiveUI captures pane, detects interactive content, and sends/updates keyboard.
func (b *Bot) handleInteractiveUI(chatID int64, threadID int, userID int64, windowID string) {
	paneText, err := tmux.CapturePane(b.config.TmuxSessionName, windowID, false)
	if err != nil {
		if tmux.IsWindowDead(err) {
			log.Printf("Interactive UI: window %s is dead", windowID)
			clearInteractiveUI(userID, threadID)
		}
		return
	}

	ui, ok := monitor.ExtractInteractiveContent(paneText)
	if !ok {
		return
	}

	keyboard := buildInteractiveKeyboard(ui.Name)
	text := formatInteractiveContent(ui)

	key := interactiveKey{userID, threadID}

	interactive.mu.RLock()
	existingMsgID, hasExisting := interactive.messages[key]
	interactive.mu.RUnlock()

	if hasExisting {
		// Edit existing message with retry
		if err := retryOnFlood(func() error {
			return b.editMessageWithKeyboard(chatID, existingMsgID, text, keyboard)
		}); err != nil {
			log.Printf("Error editing interactive message: %v", err)
		}
	} else {
		// Send new message with retry
		var msg tgbotapi.Message
		if err := retryOnFlood(func() error {
			var sendErr error
			msg, sendErr = b.sendMessageWithKeyboard(chatID, threadID, text, keyboard)
			return sendErr
		}); err != nil {
			log.Printf("Error sending interactive message after retries: %v", err)
			return
		}
		interactive.mu.Lock()
		interactive.messages[key] = msg.MessageID
		interactive.modes[key] = windowID
		interactive.mu.Unlock()
	}
}

// getInteractiveWindow returns the window ID if the user is in interactive mode.
func getInteractiveWindow(userID int64, threadID int) (string, bool) {
	key := interactiveKey{userID, threadID}
	interactive.mu.RLock()
	defer interactive.mu.RUnlock()
	wid, ok := interactive.modes[key]
	return wid, ok
}

// clearInteractiveUI removes the tracked interactive message.
func clearInteractiveUI(userID int64, threadID int) {
	key := interactiveKey{userID, threadID}
	interactive.mu.Lock()
	delete(interactive.messages, key)
	delete(interactive.modes, key)
	interactive.mu.Unlock()
}

// handleInteractiveCallback processes interactive UI navigation callbacks.
func (b *Bot) handleInteractiveCallback(cq *tgbotapi.CallbackQuery) {
	userID := cq.From.ID
	threadID := getThreadID(cq.Message)
	chatID := cq.Message.Chat.ID

	key := interactiveKey{userID, threadID}

	interactive.mu.RLock()
	windowID, ok := interactive.modes[key]
	interactive.mu.RUnlock()

	if !ok {
		return
	}

	data := cq.Data
	session := b.config.TmuxSessionName

	sendKey := func(key string) error {
		return tmux.SendSpecialKey(session, windowID, key)
	}

	var sendErr error
	switch {
	case data == "nav_up":
		sendErr = sendKey("Up")
	case data == "nav_down":
		sendErr = sendKey("Down")
	case data == "nav_left":
		sendErr = sendKey("Left")
	case data == "nav_right":
		sendErr = sendKey("Right")
	case data == "nav_space":
		sendErr = sendKey("Space")
	case data == "nav_tab":
		sendErr = sendKey("Tab")
	case data == "nav_esc":
		sendErr = sendKey("Escape")
		clearInteractiveUI(userID, threadID)
		return
	case data == "nav_enter":
		sendErr = sendKey("Enter")
		clearInteractiveUI(userID, threadID)
		return
	case data == "nav_refresh":
		// Just refresh, no key sent
	default:
		return
	}

	if sendErr != nil {
		if tmux.IsWindowDead(sendErr) {
			log.Printf("Interactive callback: window %s is dead", windowID)
			clearInteractiveUI(userID, threadID)
		}
		return
	}

	// Wait for UI to update, then refresh
	time.Sleep(300 * time.Millisecond)
	b.handleInteractiveUI(chatID, threadID, userID, windowID)
}

// retryOnFlood retries a Telegram API call with exponential backoff on 429 errors.
// Respects the retry-after header from Telegram. Retries up to 4 times.
func retryOnFlood(fn func() error) error {
	const maxRetries = 4
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		errStr := err.Error()
		if !strings.Contains(errStr, "Too Many Requests") && !strings.Contains(errStr, "429") {
			return err // non-retryable error
		}
		if attempt == maxRetries {
			return err
		}
		// Parse retry-after, default to exponential backoff
		wait := time.Duration(1<<uint(attempt)) * time.Second
		if m := interactiveRetryRe.FindStringSubmatch(errStr); len(m) == 2 {
			if secs, parseErr := strconv.Atoi(m[1]); parseErr == nil && secs > 0 {
				wait = time.Duration(secs)*time.Second + time.Second
			}
		}
		log.Printf("Interactive message rate-limited, retrying in %v (attempt %d/%d)", wait, attempt+1, maxRetries)
		time.Sleep(wait)
	}
	return nil // unreachable
}

// buildInteractiveKeyboard builds the inline keyboard for interactive navigation.
func buildInteractiveKeyboard(uiType string) tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton

	if uiType == "RestoreCheckpoint" {
		// Vertical-only layout
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("\u2191", "nav_up"),
				tgbotapi.NewInlineKeyboardButtonData("\u2193", "nav_down"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Enter", "nav_enter"),
				tgbotapi.NewInlineKeyboardButtonData("Esc", "nav_esc"),
				tgbotapi.NewInlineKeyboardButtonData("\U0001F504", "nav_refresh"),
			),
		)
	} else {
		// Full layout
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("\u2191", "nav_up"),
				tgbotapi.NewInlineKeyboardButtonData("\u2193", "nav_down"),
				tgbotapi.NewInlineKeyboardButtonData("\u2190", "nav_left"),
				tgbotapi.NewInlineKeyboardButtonData("\u2192", "nav_right"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Space", "nav_space"),
				tgbotapi.NewInlineKeyboardButtonData("Tab", "nav_tab"),
				tgbotapi.NewInlineKeyboardButtonData("Esc", "nav_esc"),
				tgbotapi.NewInlineKeyboardButtonData("Enter", "nav_enter"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("\U0001F504 Refresh", "nav_refresh"),
			),
		)
	}

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// formatInteractiveContent formats the UI content for display.
func formatInteractiveContent(ui monitor.UIContent) string {
	name := ui.Name
	// Simplify names for display
	if strings.HasPrefix(name, "AskUserQuestion") {
		name = "Question"
	} else if name == "ExitPlanMode" {
		name = "Plan Review"
	} else if name == "PermissionPrompt" {
		name = "Permission"
	} else if name == "RestoreCheckpoint" {
		name = "Restore"
	} else if name == "Settings" {
		name = "Settings"
	}

	content := monitor.ShortenSeparators(ui.Content)
	if len(content) > 3000 {
		content = content[:3000] + "\n..."
	}

	return fmt.Sprintf("[%s]\n%s", name, content)
}
