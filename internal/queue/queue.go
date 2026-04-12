package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/render"
)

const (
	maxMergeLen = 3800
	chanBufSize = 100
)

// MessageTask represents a message to send to Telegram.
type MessageTask struct {
	UserID      int64
	ThreadID    int
	ChatID      int64
	Parts       []string
	ContentType string // "content", "tool_use", "tool_result", "status_update", "status_clear"
	ToolUseID   string // for tool_result editing
	WindowID    string
}

// userThread is a composite key for per-(user, thread) tracking.
type userThread struct {
	UserID   int64
	ThreadID int
}

// StatusInfo tracks the current status message for a user+thread.
type StatusInfo struct {
	MessageID int
	WindowID  string
	Text      string
}

// Queue manages per-user message sending goroutines.
type Queue struct {
	mu         sync.RWMutex
	api        *tgbotapi.BotAPI
	queues     map[int64]chan MessageTask // user_id → channel
	toolMsgIDs map[string]toolMsgInfo    // tool_use_id → message info
	statusMsgs map[userThread]StatusInfo // (user_id, thread_id) → status message
	flood      *FloodControl
}

type toolMsgInfo struct {
	ChatID    int64
	MessageID int
	ThreadID  int
}

// New creates a new Queue.
func New(api *tgbotapi.BotAPI) *Queue {
	return &Queue{
		api:        api,
		queues:     make(map[int64]chan MessageTask),
		toolMsgIDs: make(map[string]toolMsgInfo),
		statusMsgs: make(map[userThread]StatusInfo),
		flood:      NewFloodControl(),
	}
}

// Enqueue adds a message task to the user's queue.
func (q *Queue) Enqueue(task MessageTask) {
	// Don't enqueue ephemeral messages during flood — they'd be dropped by the worker
	// anyway. This prevents the channel from filling with doomed messages, which would
	// block content messages from being enqueued.
	if q.flood.IsFlooded(task.ChatID) {
		switch task.ContentType {
		case "status_update", "status_clear", "tool_use", "tool_result":
			return
		}
	}

	q.mu.Lock()
	ch, ok := q.queues[task.UserID]
	if !ok {
		ch = make(chan MessageTask, chanBufSize)
		q.queues[task.UserID] = ch
		go q.worker(task.UserID, ch)
	}
	q.mu.Unlock()

	select {
	case ch <- task:
	case <-time.After(5 * time.Second):
		log.Printf("Queue full for user %d after 5s, dropping message (type=%s)", task.UserID, task.ContentType)
	}
}

// QueueLen returns the number of pending messages for a user.
func (q *Queue) QueueLen(userID int64) int {
	q.mu.RLock()
	ch, ok := q.queues[userID]
	q.mu.RUnlock()
	if !ok {
		return 0
	}
	return len(ch)
}

// GetStatusMessage returns the current status message for a user+thread.
func (q *Queue) GetStatusMessage(userID int64, threadID int) (StatusInfo, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	info, ok := q.statusMsgs[userThread{userID, threadID}]
	return info, ok
}

// worker processes messages for a single user.
func (q *Queue) worker(userID int64, ch chan MessageTask) {
	for task := range ch {
		q.processTask(task, ch)
	}
}

func (q *Queue) processTask(task MessageTask, ch chan MessageTask) {
	// Check flood control using chatID (flood bans are keyed by chatID, not userID)
	if q.flood.IsFlooded(task.ChatID) {
		switch task.ContentType {
		case "status_update", "status_clear", "tool_use":
			// Drop low-value messages during floods — they'll be stale by the time flood clears
			return
		case "tool_result":
			// Drop tool_result too — the tool_use message it would edit was likely dropped
			return
		default:
			// Content messages: wait for flood to clear
			q.flood.WaitIfFlooded(task.ChatID)
			// After waking, drain stale messages from the buffer
			q.drainStale(task.ChatID, ch)
		}
	}

	switch task.ContentType {
	case "content":
		q.processContent(task, ch)
	case "tool_use":
		q.processToolUse(task)
	case "tool_result":
		q.processToolResult(task)
	case "status_update":
		q.processStatusUpdate(task)
	case "status_clear":
		q.processStatusClear(task)
	default:
		q.processContent(task, ch)
	}
}

func (q *Queue) processContent(task MessageTask, ch chan MessageTask) {
	text := strings.Join(task.Parts, "\n")

	// Try to merge consecutive content tasks, collecting any non-content tasks
	var deferred []MessageTask
	text, deferred = q.mergeFromChannel2(text, task.WindowID, ch)

	// Send the merged content
	q.sendMessage(task.ChatID, task.ThreadID, text)

	// Process any deferred non-content tasks that were in the channel
	for _, dt := range deferred {
		q.processTask(dt, ch)
	}
}

func (q *Queue) processToolUse(task MessageTask) {
	text := strings.Join(task.Parts, "\n")
	msgID := q.sendMessage(task.ChatID, task.ThreadID, text)

	if msgID != 0 && task.ToolUseID != "" {
		q.mu.Lock()
		q.toolMsgIDs[task.ToolUseID] = toolMsgInfo{
			ChatID:    task.ChatID,
			MessageID: msgID,
			ThreadID:  task.ThreadID,
		}
		q.mu.Unlock()
	}
}

func (q *Queue) processToolResult(task MessageTask) {
	text := strings.Join(task.Parts, "\n")

	// Try to edit the tool_use message in-place
	q.mu.Lock()
	info, ok := q.toolMsgIDs[task.ToolUseID]
	if ok {
		delete(q.toolMsgIDs, task.ToolUseID)
	}
	q.mu.Unlock()

	if ok && info.MessageID != 0 {
		if err := q.editMessage(info.ChatID, info.MessageID, text); err != nil {
			// Fallback: send new message
			q.sendMessage(task.ChatID, task.ThreadID, text)
		}
		return
	}

	q.sendMessage(task.ChatID, task.ThreadID, text)
}

func (q *Queue) processStatusUpdate(task MessageTask) {
	text := strings.Join(task.Parts, "\n")
	ut := userThread{task.UserID, task.ThreadID}

	q.mu.RLock()
	existing, hasExisting := q.statusMsgs[ut]
	q.mu.RUnlock()

	// Deduplicate: skip if same text
	if hasExisting && existing.Text == text {
		return
	}

	// Send typing indicator when Claude is actively working (after dedup to avoid wasted API calls)
	if strings.Contains(strings.ToLower(text), "esc to interrupt") {
		q.sendTyping(task.ChatID)
	}

	if hasExisting && existing.MessageID != 0 {
		// Edit existing status message
		if err := q.editMessage(task.ChatID, existing.MessageID, text); err == nil {
			q.mu.Lock()
			q.statusMsgs[ut] = StatusInfo{
				MessageID: existing.MessageID,
				WindowID:  task.WindowID,
				Text:      text,
			}
			q.mu.Unlock()
			return
		}
	}

	// Send new status message
	msgID := q.sendMessage(task.ChatID, task.ThreadID, text)
	q.mu.Lock()
	q.statusMsgs[ut] = StatusInfo{
		MessageID: msgID,
		WindowID:  task.WindowID,
		Text:      text,
	}
	q.mu.Unlock()
}

func (q *Queue) processStatusClear(task MessageTask) {
	ut := userThread{task.UserID, task.ThreadID}

	q.mu.Lock()
	status, ok := q.statusMsgs[ut]
	if ok {
		delete(q.statusMsgs, ut)
	}
	q.mu.Unlock()

	if ok && status.MessageID != 0 {
		q.deleteMessage(task.ChatID, status.MessageID)
	}
}

// mergeFromChannel2 merges consecutive content tasks from the channel.
// Returns the merged text and any non-content tasks that were found in the channel
// (these must be processed by the caller to preserve ordering).
func (q *Queue) mergeFromChannel2(text, windowID string, ch chan MessageTask) (string, []MessageTask) {
	var deferred []MessageTask
	for {
		select {
		case next, ok := <-ch:
			if !ok {
				return text, deferred
			}
			if next.ContentType != "content" || next.WindowID != windowID {
				deferred = append(deferred, next)
				return text, deferred
			}
			nextText := strings.Join(next.Parts, "\n")
			if len(text)+len(nextText)+1 > maxMergeLen {
				deferred = append(deferred, next)
				return text, deferred
			}
			text = text + "\n" + nextText
		default:
			return text, deferred
		}
	}
}

// drainStale drains stale low-priority messages from the channel after a flood wait.
// This prevents a burst of stale tool_use/tool_result/status messages from being sent
// immediately after the flood clears, which would trigger another flood ban.
func (q *Queue) drainStale(chatID int64, ch chan MessageTask) {
	drained := 0
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			switch msg.ContentType {
			case "status_update", "status_clear", "tool_use", "tool_result":
				drained++
				continue
			default:
				// Put back content messages by processing them
				if drained > 0 {
					log.Printf("Drained %d stale messages after flood for chat %d", drained, chatID)
				}
				q.processTask(msg, ch)
				return
			}
		default:
			if drained > 0 {
				log.Printf("Drained %d stale messages after flood for chat %d", drained, chatID)
			}
			return
		}
	}
}

// IsFlooded returns true if a chat is currently flood-banned.
func (q *Queue) IsFlooded(chatID int64) bool {
	return q.flood.IsFlooded(chatID)
}

// HandleFloodError registers a flood ban for an external 429 error (e.g. from screenshot sends).
func (q *Queue) HandleFloodError(chatID int64, err error) {
	q.flood.HandleError(chatID, err)
}

// sendMessage sends a message with MarkdownV2, falling back to plain text.
// Long messages are split at newline boundaries before conversion.
// Returns the message ID of the last sent message.
func (q *Queue) sendMessage(chatID int64, threadID int, text string) int {
	parts := render.SplitMessage(text, 3000)

	var lastMsgID int
	for i, part := range parts {
		sendText := part
		// Add pagination suffix for multi-part messages
		if len(parts) > 1 {
			sendText = fmt.Sprintf("%s\n[%d/%d]", part, i+1, len(parts))
		}

		msgID := q.sendSingleMessage(chatID, threadID, sendText)
		if msgID != 0 {
			lastMsgID = msgID
		}
	}
	return lastMsgID
}

// sendSingleMessage sends a single message with MarkdownV2, falling back to plain text.
// Retries once with flood-aware backoff. Does not retry permanent errors.
func (q *Queue) sendSingleMessage(chatID int64, threadID int, text string) int {
	// Try MarkdownV2 first
	mdv2 := render.ToMarkdownV2(text)
	msgID, err := q.sendRaw(chatID, threadID, mdv2, "MarkdownV2")
	if err == nil {
		return msgID
	}

	// Don't retry permanent errors (bad thread, bad chat, etc.)
	if isPermanentError(err) {
		log.Printf("Permanent send error (chat=%d, thread=%d): %v", chatID, threadID, err)
		return 0
	}

	// Wait for flood to clear before plain text fallback
	q.flood.WaitIfFlooded(chatID)

	plain := render.ToPlainText(text)
	msgID, err = q.sendRaw(chatID, threadID, plain, "")
	if err != nil {
		log.Printf("Plain text fallback failed (chat=%d, thread=%d): %v", chatID, threadID, err)
		return 0
	}
	return msgID
}

// isPermanentError returns true for errors that should not be retried.
func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "thread not found") ||
		strings.Contains(msg, "chat not found") ||
		strings.Contains(msg, "bot was blocked") ||
		strings.Contains(msg, "not enough rights")
}

// sendRaw sends a message via Telegram API.
func (q *Queue) sendRaw(chatID int64, threadID int, text, parseMode string) (int, error) {
	q.flood.Throttle(chatID)
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", text)
	if parseMode != "" {
		params.AddNonEmpty("parse_mode", parseMode)
	}
	if threadID != 0 {
		params.AddNonZero("message_thread_id", threadID)
	}
	params.AddNonEmpty("link_preview_options", `{"is_disabled":true}`)

	resp, err := q.api.MakeRequest("sendMessage", params)
	if err != nil {
		q.flood.HandleError(chatID, err)
		return 0, err
	}

	var msg tgbotapi.Message
	json.Unmarshal(resp.Result, &msg)
	return msg.MessageID, nil
}

// editMessage edits a message, trying MarkdownV2 then plain text.
func (q *Queue) editMessage(chatID int64, messageID int, text string) error {
	mdv2 := render.ToMarkdownV2(text)
	err := q.editRaw(chatID, messageID, mdv2, "MarkdownV2")
	if err == nil {
		return nil
	}

	if isPermanentError(err) {
		return err
	}

	// Wait for flood to clear before plain text fallback
	q.flood.WaitIfFlooded(chatID)

	plain := render.ToPlainText(text)
	return q.editRaw(chatID, messageID, plain, "")
}

func (q *Queue) editRaw(chatID int64, messageID int, text, parseMode string) error {
	q.flood.Throttle(chatID)
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	params.AddNonEmpty("text", text)
	if parseMode != "" {
		params.AddNonEmpty("parse_mode", parseMode)
	}
	params.AddNonEmpty("link_preview_options", `{"is_disabled":true}`)
	_, err := q.api.MakeRequest("editMessageText", params)
	if err != nil {
		q.flood.HandleError(chatID, err)
	}
	return err
}

func (q *Queue) deleteMessage(chatID int64, messageID int) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	q.api.MakeRequest("deleteMessage", params)
}

// sendTyping sends a "typing" chat action to indicate the bot is working.
func (q *Queue) sendTyping(chatID int64) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("action", "typing")
	q.api.MakeRequest("sendChatAction", params)
}
