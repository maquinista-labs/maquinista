package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// BotAPIClient adapts tgbotapi.BotAPI to TelegramClient. It reuses the raw
// MakeRequest path so forum-topic threading works (same pattern as
// internal/bot/telegram.go's sendMessageInThread).
type BotAPIClient struct {
	API *tgbotapi.BotAPI
}

// SendMessage sends `text` to `chatID` in `threadID`, returning the Telegram
// message_id. A 429 is surfaced as *RateLimitError with the server's
// retry_after.
func (c *BotAPIClient) SendMessage(ctx context.Context, chatID int64, threadID int, text string) (int64, error) {
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonEmpty("text", text)
	if threadID != 0 {
		params.AddNonZero("message_thread_id", threadID)
	}

	resp, err := c.API.MakeRequest("sendMessage", params)
	if err != nil {
		var tgErr *tgbotapi.Error
		if errors.As(err, &tgErr) && tgErr.Code == 429 {
			return 0, &RateLimitError{RetryAfter: time.Duration(tgErr.RetryAfter) * time.Second}
		}
		return 0, err
	}
	if !resp.Ok {
		if resp.ErrorCode == 429 && resp.Parameters != nil {
			return 0, &RateLimitError{RetryAfter: time.Duration(resp.Parameters.RetryAfter) * time.Second}
		}
		return 0, fmt.Errorf("telegram: %s", resp.Description)
	}

	var msg tgbotapi.Message
	if err := json.Unmarshal(resp.Result, &msg); err != nil {
		return 0, fmt.Errorf("decode message: %w", err)
	}
	return int64(msg.MessageID), nil
}
