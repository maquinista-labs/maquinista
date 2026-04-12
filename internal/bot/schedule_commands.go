package bot

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/jobreg"
)

// handleScheduleCommand implements /schedule <name> "<cron>" <@agent> "<prompt>"
// — the invoking topic becomes the default reply_channel.
func (b *Bot) handleScheduleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}

	args, err := splitQuoted(msg.CommandArguments())
	if err != nil || len(args) < 4 {
		b.reply(chatID, threadID, `Usage: /schedule <name> "<cron>" @agent "<prompt>"`)
		return
	}
	name := args[0]
	cronExpr := args[1]
	agentID := strings.TrimPrefix(args[2], "@")
	prompt := args[3]

	userIDStr := fmt.Sprintf("%d", msg.From.ID)
	threadIDStr := fmt.Sprintf("%d", threadID)
	s := jobreg.Schedule{
		Name: name, Cron: cronExpr, AgentID: agentID,
		Prompt: map[string]any{"type": "command", "text": prompt},
		ReplyChannel: map[string]any{
			"channel": "telegram", "user_id": userIDStr,
			"thread_id": threadIDStr, "chat_id": chatID,
		},
	}
	id, err := jobreg.AddSchedule(context.Background(), pool, s)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Schedule %s created (id=%s).", name, id))
}

// handleHookRegisterCommand: /hook_register <name> <path> <secret> @agent "<template>"
func (b *Bot) handleHookRegisterCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if !b.config.IsAllowedUser(msg.From.ID) {
		b.reply(chatID, threadID, "Only ALLOWED_USERS can register hooks.")
		return
	}
	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}
	args, err := splitQuoted(msg.CommandArguments())
	if err != nil || len(args) < 5 {
		b.reply(chatID, threadID, `Usage: /hook_register <name> <path> <secret> @agent "<template>"`)
		return
	}
	h := jobreg.Hook{
		Name: args[0], Path: args[1], Secret: args[2],
		AgentID: strings.TrimPrefix(args[3], "@"), PromptTemplate: args[4],
	}
	id, err := jobreg.AddHook(context.Background(), pool, h)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	b.reply(chatID, threadID, fmt.Sprintf("Hook %s registered (id=%s).", h.Name, id))
}

func (b *Bot) handleHookEnableCommand(msg *tgbotapi.Message) { b.toggleHook(msg, true) }
func (b *Bot) handleHookDisableCommand(msg *tgbotapi.Message) { b.toggleHook(msg, false) }

func (b *Bot) toggleHook(msg *tgbotapi.Message, enabled bool) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}
	name := strings.TrimSpace(msg.CommandArguments())
	if name == "" {
		b.reply(chatID, threadID, "Usage: /hook_enable <name> | /hook_disable <name>")
		return
	}
	if err := jobreg.SetHookEnabled(context.Background(), pool, name, enabled); err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	b.reply(chatID, threadID, fmt.Sprintf("Hook %s %s.", name, state))
}

// splitQuoted splits on whitespace but respects double quotes. Returns an
// error if a quote is left unclosed.
func splitQuoted(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t') && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unclosed quote")
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}
