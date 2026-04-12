package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/jobreg"
)

// handleJobsCommand: /jobs — list scheduled_jobs rows.
func (b *Bot) handleJobsCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}
	list, err := jobreg.ListSchedules(context.Background(), pool)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(list) == 0 {
		b.reply(chatID, threadID, "No scheduled jobs.")
		return
	}
	var lines []string
	lines = append(lines, "Scheduled jobs:")
	for _, s := range list {
		status := "ON"
		if !s.Enabled {
			status = "off"
		}
		lines = append(lines, fmt.Sprintf("• %s — %s → @%s [%s] next: %s",
			s.Name, s.Cron, s.AgentID, status, s.NextRunAt.Format("2006-01-02 15:04 MST")))
	}
	b.reply(chatID, threadID, strings.Join(lines, "\n"))
}

// handleHooksCommand: /hooks — list webhook_handlers rows.
func (b *Bot) handleHooksCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}
	list, err := jobreg.ListHooks(context.Background(), pool)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(list) == 0 {
		b.reply(chatID, threadID, "No webhook handlers.")
		return
	}
	var lines []string
	lines = append(lines, "Webhook handlers:")
	for _, h := range list {
		status := "ON"
		if !h.Enabled {
			status = "off"
		}
		lines = append(lines, fmt.Sprintf("• %s — %s → @%s [%s]", h.Name, h.Path, h.AgentID, status))
	}
	b.reply(chatID, threadID, strings.Join(lines, "\n"))
}

// handleJobRunsCommand: /job_runs <name> [page] — last N runs of a source.
func (b *Bot) handleJobRunsCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available.")
		return
	}
	args := strings.Fields(msg.CommandArguments())
	if len(args) == 0 {
		b.reply(chatID, threadID, "Usage: /job_runs <name> [page]")
		return
	}
	name := args[0]
	page := 0
	if len(args) >= 2 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p - 1
		}
	}
	const perPage = 25
	runs, err := jobreg.JobRunsByName(context.Background(), pool, name, perPage, page*perPage)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(runs) == 0 {
		b.reply(chatID, threadID, fmt.Sprintf("No runs for %q (page %d).", name, page+1))
		return
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Runs for %s (page %d):", name, page+1))
	for _, r := range runs {
		errSuffix := ""
		if r.LastError != nil && *r.LastError != "" {
			errSuffix = fmt.Sprintf(" — %s", *r.LastError)
		}
		lines = append(lines, fmt.Sprintf("• %s [%s]%s",
			r.EnqueuedAt.Format("2006-01-02 15:04"), r.Status, errSuffix))
	}
	b.reply(chatID, threadID, strings.Join(lines, "\n"))
}
