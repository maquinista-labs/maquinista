package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/routing"
)

// handleAgentDefaultCommand implements /agent_default @handle — attach the
// current topic to an already-running agent. Unknown handle returns a
// guidance error; never auto-spawns (creation happens via tier-3 on a
// regular message per per-topic-agent-pivot.md).
func (b *Bot) handleAgentDefaultCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use /agent_default.")
		return
	}

	token := parseAgentArg(msg.CommandArguments())
	if token == "" {
		b.reply(chatID, threadID, "Usage: /agent_default @handle")
		return
	}

	userID := strconv.FormatInt(msg.From.ID, 10)
	threadIDStr := strconv.Itoa(threadID)
	chat := chatID
	res, err := routing.SetUserDefault(context.Background(), pool, userID, threadIDStr, &chat, token)
	if err != nil {
		if errors.Is(err, routing.ErrUnknownAgent) {
			b.reply(chatID, threadID, fmt.Sprintf(
				"No agent @%s. Use /agent_list to see existing agents, or send a message in a fresh topic to spawn one (rename afterwards with /agent_rename <handle>).",
				token))
			return
		}
		b.reply(chatID, threadID, fmt.Sprintf("Error setting default: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Owner binding set: @%s for this topic.", res.AgentID))
}

// handleAgentRenameCommand implements /agent_rename <handle> — set the
// handle on the current topic's owner agent. Validates the handle format,
// rejects reserved-prefix 't-', and enforces uniqueness via the DB index.
func (b *Bot) handleAgentRenameCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use /agent_rename.")
		return
	}

	handle := parseAgentArg(msg.CommandArguments())
	if handle == "" {
		b.reply(chatID, threadID, "Usage: /agent_rename <handle>  (2–32 chars, [a-z0-9_-], not starting with 't-')")
		return
	}

	userID := strconv.FormatInt(msg.From.ID, 10)
	threadIDStr := strconv.Itoa(threadID)

	// Find the owner agent for this topic. Rename only makes sense once
	// tier-3 has spawned one.
	windowID, bound := b.state.GetWindowForThread(userID, threadIDStr)
	if !bound {
		b.reply(chatID, threadID, "No agent bound to this topic yet. Send a message first to spawn one.")
		return
	}
	// state.ThreadBindings carries the window id; the agent id lives in the
	// DB. Resolve agent via the routing helper (id-or-handle, though here
	// we're starting from tmux_window).
	ctx := context.Background()
	var agentID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agents WHERE tmux_window=$1 LIMIT 1`, windowID).Scan(&agentID); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Could not resolve agent for this topic: %v", err))
		return
	}

	if err := routing.SetHandle(ctx, pool, agentID, handle); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Rename failed: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Handle set: @%s  (you can now mention this agent from any topic.)", strings.ToLower(handle)))
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
