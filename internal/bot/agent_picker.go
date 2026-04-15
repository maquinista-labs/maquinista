package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/routing"
)

// agentPickerState holds per-user tier-4 picker state. The pending text is
// held here and replayed to the inbox once the user selects an agent.
type agentPickerState struct {
	AgentIDs    []string
	PendingText string
	MessageID   int
	ChatID      int64
	ThreadID    int
}

// showAgentPicker presents the tier-4 picker listing live agents. The user's
// pending message is stashed in state and replayed after they tap an option.
// Empty agent list is a legitimate outcome — the bot tells the user how to
// start one and does not trap the message.
func (b *Bot) showAgentPicker(chatID int64, threadID int, userID int64, pendingText string) {
	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Error: agent mailbox unavailable (DATABASE_URL).")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3e9)
	defer cancel()
	rows, err := pool.Query(ctx, `
		SELECT id FROM agents
		WHERE status IN ('running', 'working', 'idle')
		ORDER BY id
	`)
	if err != nil {
		log.Printf("agent picker: query: %v", err)
		b.reply(chatID, threadID, "Error listing agents.")
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Printf("agent picker: scan: %v", err)
			continue
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		b.reply(chatID, threadID,
			"No agents are running. Start one with:\n"+
				"`AGENT_ID=<your-agent> claude`\n"+
				"then resend your message.")
		return
	}

	text, keyboard := buildAgentPicker(ids)
	msg, err := b.sendMessageWithKeyboard(chatID, threadID, text, keyboard)
	if err != nil {
		log.Printf("agent picker: send: %v", err)
		return
	}

	b.mu.Lock()
	b.agentPickerStates[userID] = &agentPickerState{
		AgentIDs:    ids,
		PendingText: pendingText,
		MessageID:   msg.MessageID,
		ChatID:      chatID,
		ThreadID:    threadID,
	}
	b.mu.Unlock()
}

func buildAgentPicker(ids []string) (string, tgbotapi.InlineKeyboardMarkup) {
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < len(ids); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			truncateName(ids[i], 30),
			fmt.Sprintf("apick_sel:%d", i),
		))
		if i+1 < len(ids) {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(
				truncateName(ids[i+1], 30),
				fmt.Sprintf("apick_sel:%d", i+1),
			))
		}
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("Cancel", "apick_cancel"),
	})
	return "Pick an agent to own this topic:", tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// processAgentPickerCallback dispatches apick_* callbacks.
func (b *Bot) processAgentPickerCallback(cq *tgbotapi.CallbackQuery) {
	userID := cq.From.ID
	data := cq.Data

	b.mu.RLock()
	st, ok := b.agentPickerStates[userID]
	b.mu.RUnlock()
	if !ok {
		return
	}

	threadID := getThreadID(cq.Message)
	if threadID != st.ThreadID {
		return
	}

	switch {
	case strings.HasPrefix(data, "apick_sel:"):
		b.handleAgentPickSel(cq, st, userID)
	case data == "apick_cancel":
		b.handleAgentPickCancel(cq, st, userID)
	}
}

func (b *Bot) handleAgentPickSel(cq *tgbotapi.CallbackQuery, st *agentPickerState, userID int64) {
	idxStr := strings.TrimPrefix(cq.Data, "apick_sel:")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(st.AgentIDs) {
		return
	}

	agentID := st.AgentIDs[idx]
	pending := st.PendingText
	chatID := st.ChatID
	messageID := st.MessageID
	threadID := st.ThreadID

	b.mu.Lock()
	delete(b.agentPickerStates, userID)
	b.mu.Unlock()

	pool := b.getPool()
	if pool == nil {
		b.editMessageText(chatID, messageID, "Error: DB unavailable.")
		return
	}

	userIDStr := strconv.FormatInt(userID, 10)
	threadIDStr := strconv.Itoa(threadID)
	ctx, cancel := context.WithTimeout(context.Background(), 3e9)
	defer cancel()
	chatIDCopy := chatID
	if _, err := routing.ConfirmPickerChoice(ctx, pool, userIDStr, threadIDStr, &chatIDCopy, agentID); err != nil {
		log.Printf("agent picker: ConfirmPickerChoice: %v", err)
		b.editMessageText(chatID, messageID, fmt.Sprintf("Error binding: %v", err))
		return
	}

	b.editMessageText(chatID, messageID, fmt.Sprintf("Bound @%s to this topic.", agentID))

	if pending == "" {
		return
	}
	// Replay the original message through the inbox. getThreadID resolves
	// via an internal MessageID→thread cache, so reusing the picker's
	// message id is sufficient; we just need a stable From/Chat/Text.
	replay := &tgbotapi.Message{
		MessageID: cq.Message.MessageID,
		From:      cq.From,
		Chat:      cq.Message.Chat,
		Text:      pending,
	}
	if !b.routeTextViaInbox(replay, agentID, pending) {
		b.reply(chatID, threadID, "Routed binding, but failed to replay message. Please resend.")
	}
}

func (b *Bot) handleAgentPickCancel(cq *tgbotapi.CallbackQuery, st *agentPickerState, userID int64) {
	b.mu.Lock()
	delete(b.agentPickerStates, userID)
	b.mu.Unlock()
	b.editMessageText(st.ChatID, st.MessageID, "Cancelled.")
}
