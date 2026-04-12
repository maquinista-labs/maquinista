package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// routeTextViaInbox writes msg to agent_inbox as an idempotent row keyed by
// (origin_channel='telegram', external_msg_id=update_id). Returns true when
// the row was enqueued or collapsed to an existing row (idempotent replay);
// false when the DB path is unavailable so the caller can fall back to
// tmux.SendKeysWithDelay.
func (b *Bot) routeTextViaInbox(msg *tgbotapi.Message, agentID, text string) bool {
	pool := b.getPool()
	if pool == nil {
		return false
	}

	userID := strconv.FormatInt(msg.From.ID, 10)
	threadID := strconv.Itoa(getThreadID(msg))
	chatID := msg.Chat.ID
	extMsgID := strconv.Itoa(msg.MessageID)
	if msg.From != nil && msg.From.ID != 0 {
		// Telegram's update_id is only available at the Update level, not
		// on the Message; message_id is stable-per-chat, which suffices as
		// an idempotency key when paired with origin_channel='telegram'.
		// Prefix with chat id so cross-chat replays with the same message_id
		// never collide.
		extMsgID = fmt.Sprintf("%d:%s", chatID, extMsgID)
	}

	content, err := json.Marshal(map[string]string{"type": "text", "text": text})
	if err != nil {
		log.Printf("mailbox.inbound: marshal: %v", err)
		return false
	}

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Printf("mailbox.inbound: begin: %v", err)
		return false
	}
	defer tx.Rollback(ctx)

	_, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:        agentID,
		FromKind:       "user",
		FromID:         userID,
		OriginChannel:  "telegram",
		OriginUserID:   userID,
		OriginThreadID: threadID,
		OriginChatID:   &chatID,
		ExternalMsgID:  extMsgID,
		Content:        content,
	})
	if err != nil {
		log.Printf("mailbox.inbound: enqueue: %v", err)
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("mailbox.inbound: commit: %v", err)
		return false
	}
	return true
}
