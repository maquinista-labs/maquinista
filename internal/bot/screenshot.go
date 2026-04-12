package bot

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/render"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// screenshotState tracks the current screenshot message per (user, thread).
type screenshotState struct {
	ChatID    int64
	MessageID int
	WindowID  string
}

var (
	screenshotStates   = make(map[string]*screenshotState) // "userID:threadID" → state
	screenshotStatesMu sync.Mutex
)

func screenshotKey(userID int64, threadID int) string {
	return fmt.Sprintf("%d:%d", userID, threadID)
}

// ssKeyMap maps callback key IDs to tmux key names.
var ssKeyMap = map[string]string{
	"up":    "Up",
	"down":  "Down",
	"left":  "Left",
	"right": "Right",
	"space": "Space",
	"tab":   "Tab",
	"esc":   "Escape",
	"enter": "Enter",
}

// buildScreenshotKeyboard builds inline keyboard for screenshot control.
func buildScreenshotKeyboard(windowID string) tgbotapi.InlineKeyboardMarkup {
	btn := func(label, action string) tgbotapi.InlineKeyboardButton {
		data := formatSSCallback(action, windowID)
		return tgbotapi.NewInlineKeyboardButtonData(label, data)
	}

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			btn("↑", "up"),
			btn("↓", "down"),
			btn("←", "left"),
			btn("→", "right"),
		),
		tgbotapi.NewInlineKeyboardRow(
			btn("Space", "space"),
			btn("Tab", "tab"),
			btn("Esc", "esc"),
			btn("Enter", "enter"),
		),
		tgbotapi.NewInlineKeyboardRow(
			btn("Refresh", "refresh"),
		),
	)
}

// handleScreenshotCommand captures the tmux pane and sends a PNG screenshot.
func (b *Bot) handleScreenshotCommand(msg *tgbotapi.Message) {
	windowID, bound := b.resolveWindow(msg)
	if !bound {
		b.reply(msg.Chat.ID, getThreadID(msg), "No session bound to this topic.")
		return
	}

	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	// Check flood control before doing expensive work
	if b.msgQueue != nil && b.msgQueue.IsFlooded(chatID) {
		b.reply(chatID, threadID, "Rate limited by Telegram. Try again in a moment.")
		return
	}

	paneText, err := tmux.CapturePane(b.config.TmuxSessionName, windowID, true)
	if err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, "")
			return
		}
		log.Printf("Error capturing pane for screenshot: %v", err)
		b.reply(chatID, threadID, "Error: failed to capture pane.")
		return
	}

	pngData, err := render.RenderScreenshot(paneText)
	if err != nil {
		log.Printf("Error rendering screenshot: %v", err)
		b.reply(chatID, threadID, "Error: failed to render screenshot.")
		return
	}

	keyboard := buildScreenshotKeyboard(windowID)
	sentMsg, err := b.sendDocumentInThread(chatID, threadID, pngData, "screenshot.png", keyboard)
	if err != nil {
		log.Printf("Error sending screenshot: %v", err)
		// Register flood ban so queue and future screenshots respect it
		if b.msgQueue != nil {
			b.msgQueue.HandleFloodError(chatID, err)
		}
		return
	}

	// Track this screenshot message
	screenshotStatesMu.Lock()
	screenshotStates[screenshotKey(msg.From.ID, threadID)] = &screenshotState{
		ChatID:    chatID,
		MessageID: sentMsg.MessageID,
		WindowID:  windowID,
	}
	screenshotStatesMu.Unlock()
}

// handleScreenshotCB handles screenshot control callbacks.
func (b *Bot) handleScreenshotCB(cq *tgbotapi.CallbackQuery) {
	action, windowID, ok := parseSSCallbackData(cq.Data)
	if !ok {
		return
	}

	// Check flood control before hitting Telegram API
	if b.msgQueue != nil && cq.Message != nil && b.msgQueue.IsFlooded(cq.Message.Chat.ID) {
		return
	}

	if action == "refresh" {
		b.refreshScreenshot(cq, windowID)
		return
	}

	tmuxKey, ok := ssKeyMap[action]
	if !ok {
		return
	}

	// Send key to tmux
	if err := tmux.SendSpecialKey(b.config.TmuxSessionName, windowID, tmuxKey); err != nil {
		if tmux.IsWindowDead(err) {
			log.Printf("Screenshot callback: window %s is dead", windowID)
		} else {
			log.Printf("Error sending key %s to %s: %v", tmuxKey, windowID, err)
		}
		return
	}

	// Wait for terminal to update
	time.Sleep(500 * time.Millisecond)

	// Refresh the screenshot
	b.refreshScreenshot(cq, windowID)
}

// refreshScreenshot captures, renders, and edits the screenshot message.
func (b *Bot) refreshScreenshot(cq *tgbotapi.CallbackQuery, windowID string) {
	paneText, err := tmux.CapturePane(b.config.TmuxSessionName, windowID, true)
	if err != nil {
		if tmux.IsWindowDead(err) {
			log.Printf("Screenshot refresh: window %s is dead", windowID)
		} else {
			log.Printf("Error capturing pane for refresh: %v", err)
		}
		return
	}

	pngData, err := render.RenderScreenshot(paneText)
	if err != nil {
		log.Printf("Error rendering screenshot for refresh: %v", err)
		return
	}

	chatID := cq.Message.Chat.ID
	messageID := cq.Message.MessageID
	keyboard := buildScreenshotKeyboard(windowID)

	if err := b.editMessageMedia(chatID, messageID, pngData, "screenshot.png", keyboard); err != nil {
		log.Printf("Error editing screenshot message: %v", err)
		if b.msgQueue != nil {
			b.msgQueue.HandleFloodError(chatID, err)
		}
	}
}

// sendDocumentInThread sends a document (file bytes) in a forum thread with an inline keyboard.
// Uses raw UploadFiles API because go-telegram-bot-api v5 doesn't support message_thread_id.
func (b *Bot) sendDocumentInThread(chatID int64, threadID int, data []byte, filename string, keyboard tgbotapi.InlineKeyboardMarkup) (tgbotapi.Message, error) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	if threadID != 0 {
		params.AddNonZero("message_thread_id", threadID)
	}
	if len(keyboard.InlineKeyboard) > 0 {
		kbJSON, _ := json.Marshal(keyboard)
		params["reply_markup"] = string(kbJSON)
	}

	file := tgbotapi.FileBytes{Name: filename, Bytes: data}

	resp, err := b.api.UploadFiles("sendDocument", params, []tgbotapi.RequestFile{
		{Name: "document", Data: file},
	})
	if err != nil {
		return tgbotapi.Message{}, fmt.Errorf("sendDocument: %w", err)
	}

	var msg tgbotapi.Message
	json.Unmarshal(resp.Result, &msg)
	return msg, nil
}

// editMessageMedia edits a document message with new media using the Telegram API.
// Uses raw UploadFiles API because go-telegram-bot-api v5 doesn't support editMessageMedia.
func (b *Bot) editMessageMedia(chatID int64, messageID int, data []byte, filename string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	kbJSON, _ := json.Marshal(keyboard)

	media := map[string]string{
		"type":  "document",
		"media": "attach://document",
	}
	mediaJSON, _ := json.Marshal(media)

	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	params["media"] = string(mediaJSON)
	params["reply_markup"] = string(kbJSON)

	file := tgbotapi.FileBytes{Name: filename, Bytes: data}

	_, err := b.api.UploadFiles("editMessageMedia", params, []tgbotapi.RequestFile{
		{Name: "document", Data: file},
	})
	if err != nil {
		return fmt.Errorf("editMessageMedia: %w", err)
	}
	return nil
}

// parseSSCallbackData parses screenshot callback data "ss_action:windowID".
func parseSSCallbackData(data string) (action, windowID string, ok bool) {
	if !strings.HasPrefix(data, "ss_") {
		return "", "", false
	}
	rest := data[3:]
	colonIdx := strings.Index(rest, ":")
	if colonIdx < 0 {
		return "", "", false
	}
	return rest[:colonIdx], rest[colonIdx+1:], true
}

// formatSSCallback builds a callback data string for a screenshot action.
func formatSSCallback(action, windowID string) string {
	data := fmt.Sprintf("ss_%s:%s", action, windowID)
	if len(data) > 64 {
		data = data[:64]
	}
	return data
}

// screenshotCallbackActions returns all valid screenshot callback actions.
func screenshotCallbackActions() []string {
	return []string{"up", "down", "left", "right", "space", "tab", "esc", "enter", "refresh"}
}
