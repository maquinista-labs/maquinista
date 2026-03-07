package bot

// Telegram forum topic support not in go-telegram-bot-api v5.5.1.
// We extract these fields from raw JSON updates.

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ForumTopicClosed represents a service message about a forum topic closed.
type ForumTopicClosed struct{}

// threadIDCache stores message_id → thread_id mappings extracted from raw JSON.
// The go-telegram-bot-api v5 library doesn't support forum topics, so we extract
// these fields ourselves from the raw update JSON.
var (
	threadIDCache   = make(map[int]int)    // message_id → thread_id
	topicClosedSet  = make(map[int]bool)   // message_id → is_topic_closed
	topicNameCache  = make(map[int]string) // thread_id → topic_name
	threadCacheMu   sync.RWMutex
)

// ForumTopicCreated represents a service message about a forum topic being created.
type ForumTopicCreated struct {
	Name string `json:"name"`
}

// rawMessage is used to extract forum-topic fields from raw update JSON.
type rawMessage struct {
	MessageID        int                `json:"message_id"`
	MessageThreadID  int                `json:"message_thread_id"`
	ForumTopicClosed *ForumTopicClosed  `json:"forum_topic_closed"`
	ReplyToMessage   *rawReplyMessage   `json:"reply_to_message"`
}

// rawReplyMessage extracts forum_topic_created from reply_to_message.
type rawReplyMessage struct {
	ForumTopicCreated *ForumTopicCreated `json:"forum_topic_created"`
}

type rawUpdate struct {
	Message       *rawMessage `json:"message"`
	CallbackQuery *struct {
		Message *rawMessage `json:"message"`
	} `json:"callback_query"`
}

// extractForumFields parses raw update JSON to cache thread IDs and topic close events.
func extractForumFields(data []byte) {
	var raw rawUpdate
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	threadCacheMu.Lock()
	defer threadCacheMu.Unlock()

	if raw.Message != nil {
		if raw.Message.MessageThreadID != 0 {
			threadIDCache[raw.Message.MessageID] = raw.Message.MessageThreadID
		}
		if raw.Message.ForumTopicClosed != nil {
			topicClosedSet[raw.Message.MessageID] = true
		}
		if raw.Message.MessageThreadID != 0 && raw.Message.ReplyToMessage != nil &&
			raw.Message.ReplyToMessage.ForumTopicCreated != nil &&
			raw.Message.ReplyToMessage.ForumTopicCreated.Name != "" {
			topicNameCache[raw.Message.MessageThreadID] = raw.Message.ReplyToMessage.ForumTopicCreated.Name
		}
	}
	if raw.CallbackQuery != nil && raw.CallbackQuery.Message != nil {
		if raw.CallbackQuery.Message.MessageThreadID != 0 {
			threadIDCache[raw.CallbackQuery.Message.MessageID] = raw.CallbackQuery.Message.MessageThreadID
		}
	}
}

// getThreadID returns the thread ID for a message.
func getThreadID(msg *tgbotapi.Message) int {
	if msg == nil {
		return 0
	}
	threadCacheMu.RLock()
	defer threadCacheMu.RUnlock()
	return threadIDCache[msg.MessageID]
}

// isForumTopicClosed checks if a message is a forum topic closed event.
func isForumTopicClosed(msg *tgbotapi.Message) bool {
	if msg == nil {
		return false
	}
	threadCacheMu.RLock()
	defer threadCacheMu.RUnlock()
	return topicClosedSet[msg.MessageID]
}

// getTopicName returns the cached topic name for a thread ID, if available.
func getTopicName(threadID int) string {
	threadCacheMu.RLock()
	defer threadCacheMu.RUnlock()
	return topicNameCache[threadID]
}

// cleanupCache removes entries for old message IDs to prevent unbounded growth.
// Note: topicNameCache is keyed by thread_id (not message_id) and is not cleaned
// here since thread IDs are long-lived and the cache is small.
func cleanupCache(keepAbove int) {
	threadCacheMu.Lock()
	defer threadCacheMu.Unlock()
	for id := range threadIDCache {
		if id < keepAbove {
			delete(threadIDCache, id)
		}
	}
	for id := range topicClosedSet {
		if id < keepAbove {
			delete(topicClosedSet, id)
		}
	}
}

// getUpdatesRaw fetches updates and returns both parsed updates and raw JSON.
func (b *Bot) getUpdatesRaw(offset, timeout int) ([]tgbotapi.Update, error) {
	params := tgbotapi.Params{}
	params.AddNonZero("offset", offset)
	params.AddNonZero("timeout", timeout)
	params["allowed_updates"] = `["message","callback_query"]`

	resp, err := b.api.MakeRequest("getUpdates", params)
	if err != nil {
		return nil, err
	}

	// Extract forum fields from raw JSON
	var rawUpdates []json.RawMessage
	if err := json.Unmarshal(resp.Result, &rawUpdates); err != nil {
		log.Printf("Error parsing raw updates: %v", err)
	} else {
		for _, raw := range rawUpdates {
			extractForumFields(raw)
		}
	}

	// Parse into standard updates
	var updates []tgbotapi.Update
	if err := json.Unmarshal(resp.Result, &updates); err != nil {
		return nil, err
	}

	return updates, nil
}

// sendMessageInThread sends a text message in a specific forum thread.
func (b *Bot) sendMessageInThread(chatID int64, threadID int, text string) (tgbotapi.Message, error) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", text)
	if threadID != 0 {
		params.AddNonZero("message_thread_id", threadID)
	}

	resp, err := b.api.MakeRequest("sendMessage", params)
	if err != nil {
		if b.msgQueue != nil {
			b.msgQueue.HandleFloodError(chatID, err)
		}
		return tgbotapi.Message{}, err
	}

	var msg tgbotapi.Message
	json.Unmarshal(resp.Result, &msg)
	return msg, nil
}

// sendMessageInThreadMD sends a MarkdownV2 message in a specific forum thread.
func (b *Bot) sendMessageInThreadMD(chatID int64, threadID int, text string) (tgbotapi.Message, error) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", text)
	params.AddNonEmpty("parse_mode", "MarkdownV2")
	if threadID != 0 {
		params.AddNonZero("message_thread_id", threadID)
	}

	resp, err := b.api.MakeRequest("sendMessage", params)
	if err != nil {
		return tgbotapi.Message{}, err
	}

	var msg tgbotapi.Message
	json.Unmarshal(resp.Result, &msg)
	return msg, nil
}

// editMessageText edits a text message.
func (b *Bot) editMessageText(chatID int64, messageID int, text string) error {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	params.AddNonEmpty("text", text)
	_, err := b.api.MakeRequest("editMessageText", params)
	return err
}

// sendMessageWithKeyboard sends a message with inline keyboard in a thread.
func (b *Bot) sendMessageWithKeyboard(chatID int64, threadID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) (tgbotapi.Message, error) {
	kbJSON, _ := json.Marshal(keyboard)

	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", text)
	if threadID != 0 {
		params.AddNonZero("message_thread_id", threadID)
	}
	params["reply_markup"] = string(kbJSON)

	resp, err := b.api.MakeRequest("sendMessage", params)
	if err != nil {
		return tgbotapi.Message{}, err
	}

	var msg tgbotapi.Message
	json.Unmarshal(resp.Result, &msg)
	return msg, nil
}

// editMessageWithKeyboard edits a message with new text and keyboard.
func (b *Bot) editMessageWithKeyboard(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	kbJSON, _ := json.Marshal(keyboard)

	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	params.AddNonEmpty("text", text)
	params["reply_markup"] = string(kbJSON)
	_, err := b.api.MakeRequest("editMessageText", params)
	return err
}

// deleteMessage deletes a message.
func (b *Bot) deleteMessage(chatID int64, messageID int) error {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	_, err := b.api.MakeRequest("deleteMessage", params)
	return err
}

// createForumTopic creates a new forum topic and returns the thread ID.
func (b *Bot) createForumTopic(chatID int64, name string) (int, error) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("name", name)

	resp, err := b.api.MakeRequest("createForumTopic", params)
	if err != nil {
		return 0, err
	}

	var result struct {
		MessageThreadID int `json:"message_thread_id"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return 0, fmt.Errorf("parsing createForumTopic response: %w", err)
	}
	return result.MessageThreadID, nil
}

// closeForumTopic closes a forum topic.
func (b *Bot) closeForumTopic(chatID int64, threadID int) error {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_thread_id", threadID)
	_, err := b.api.MakeRequest("closeForumTopic", params)
	return err
}
