package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/routing"
)

// handleDefaultCommand implements /default @agent — set the user's owner
// binding for this topic (§8.1 tier 2 override).
func (b *Bot) handleDefaultCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use /default.")
		return
	}

	agentID := parseAgentArg(msg.CommandArguments())
	if agentID == "" {
		b.reply(chatID, threadID, "Usage: /default @agent_id")
		return
	}

	userID := strconv.FormatInt(msg.From.ID, 10)
	threadIDStr := strconv.Itoa(threadID)
	chat := chatID
	if err := routing.SetUserDefault(context.Background(), pool, userID, threadIDStr, &chat, agentID); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error setting default: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Owner binding set: @%s for this topic.", agentID))
}

// handleGlobalDefaultCommand implements /global_default @agent —
// admin-only (enforced via AllowedUsers) tier-3 global default.
func (b *Bot) handleGlobalDefaultCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if !b.config.IsAllowedUser(msg.From.ID) {
		b.reply(chatID, threadID, "Only configured ALLOWED_USERS can change the global default.")
		return
	}

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}

	agentID := parseAgentArg(msg.CommandArguments())
	if agentID == "" {
		b.reply(chatID, threadID, "Usage: /global_default @agent_id")
		return
	}

	if err := routing.SetGlobalDefault(context.Background(), pool, agentID); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error setting global default: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Global default agent set to @%s.", agentID))
}

// parseAgentArg strips a leading @ if present and trims whitespace.
func parseAgentArg(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	// Cut at first whitespace — ignore anything after the agent id.
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return s
}
