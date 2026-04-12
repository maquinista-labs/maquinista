package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/runner"
)

// handleAgentListCommand shows all registered agents.
func (b *Bot) handleAgentListCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use agent commands.")
		return
	}

	agents, err := db.ListAgents(pool)
	if err != nil {
		log.Printf("Error listing agents: %v", err)
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(agents) == 0 {
		b.reply(chatID, threadID, "No agents registered.")
		return
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Agents (%d):", len(agents)))
	for _, a := range agents {
		taskStr := "—"
		if a.TaskID != nil {
			taskStr = *a.TaskID
		}
		dur := time.Since(a.StartedAt).Truncate(time.Second)
		lines = append(lines, fmt.Sprintf("  %s  %s  %s  %s  %s  %s",
			a.ID, a.RunnerType, a.Role, a.Status, taskStr, dur))
	}

	b.reply(chatID, threadID, strings.Join(lines, "\n"))
}

// handleAgentSpawnCommand spawns a new execution agent.
// Usage: /agent_spawn [name] [runner]
// Examples: /agent_spawn, /agent_spawn myagent, /agent_spawn myagent opencode
func (b *Bot) handleAgentSpawnCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use agent commands.")
		return
	}

	// Parse arguments: [name] [runner]
	args := strings.Fields(strings.TrimSpace(msg.CommandArguments()))
	agentName := fmt.Sprintf("agent-%d", time.Now().UnixNano())
	var explicitRunner runner.AgentRunner

	if len(args) >= 1 {
		agentName = args[0]
	}
	if len(args) >= 2 {
		r, err := runner.Get(args[1])
		if err != nil {
			b.reply(chatID, threadID, fmt.Sprintf("Unknown runner %q. Available: claude, opencode", args[1]))
			return
		}
		explicitRunner = r
	}

	env := map[string]string{
		"DATABASE_URL": b.config.DatabaseURL,
	}

	// Determine runner: explicit arg > default runner
	r := explicitRunner
	if r == nil {
		r = b.DefaultRunner()
	}

	a, err := agent.Spawn(pool, b.config.TmuxSessionName, agentName, "", env, r, "executor")

	if err != nil {
		log.Printf("Error spawning agent %s: %v", agentName, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error spawning agent: %v", err))
		return
	}

	runnerName := "claude"
	if r != nil {
		runnerName = r.Name()
	}
	b.reply(chatID, threadID, fmt.Sprintf("Agent spawned: %s (runner: %s)", a.ID, runnerName))
}

// handleAgentKillCommand kills a specific agent.
func (b *Bot) handleAgentKillCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use agent commands.")
		return
	}

	partialID := strings.TrimSpace(msg.CommandArguments())
	if partialID == "" {
		b.reply(chatID, threadID, "Usage: /agent_kill <id>")
		return
	}

	// Resolve partial ID
	agentID, err := resolveAgentID(pool, partialID)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}

	if err := agent.Kill(pool, b.config.TmuxSessionName, agentID); err != nil {
		log.Printf("Error killing agent %s: %v", agentID, err)
		b.reply(chatID, threadID, fmt.Sprintf("Error killing agent: %v", err))
		return
	}

	b.reply(chatID, threadID, fmt.Sprintf("Killed agent: %s", agentID))
}

// handleAgentKillAllCommand kills all agents with confirmation.
func (b *Bot) handleAgentKillAllCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, threadID, "Database not available. Set DATABASE_URL to use agent commands.")
		return
	}

	agents, err := db.ListAgents(pool)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(agents) == 0 {
		b.reply(chatID, threadID, "No agents to kill.")
		return
	}

	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("Kill all %d agents", len(agents)), "agent_killall_confirm"),
			tgbotapi.NewInlineKeyboardButtonData("Cancel", "noop"),
		),
	)

	b.sendMessageWithKeyboard(chatID, threadID,
		fmt.Sprintf("Kill all %d agents?", len(agents)), kb)
}

// processAgentCallback handles agent-related inline keyboard callbacks.
func (b *Bot) processAgentCallback(cq *tgbotapi.CallbackQuery, data string) {
	chatID := cq.Message.Chat.ID
	threadID := getThreadID(cq.Message)

	switch data {
	case "agent_killall_confirm":
		pool := b.getPool()
		if pool == nil {
			b.reply(chatID, threadID, "Database not available.")
			return
		}
		if err := agent.KillAll(pool, b.config.TmuxSessionName); err != nil {
			b.reply(chatID, threadID, fmt.Sprintf("Error: %v", err))
			return
		}
		b.editMessageText(chatID, cq.Message.MessageID, "All agents killed.")
	}
}

// handleRunnerCommand shows or switches the default runner.
// Usage: /runner [claude|opencode]
func (b *Bot) handleRunnerCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	arg := strings.TrimSpace(msg.CommandArguments())
	if arg == "" {
		// Show current runner
		current := "claude"
		if r := b.DefaultRunner(); r != nil {
			current = r.Name()
		}
		available := []string{}
		for name, r := range runner.Runners() {
			marker := ""
			if name == current {
				marker = " (active)"
			}
			installed := ""
			if !r.DetectInstallation() {
				installed = " [not installed]"
			}
			available = append(available, fmt.Sprintf("  %s%s%s", name, marker, installed))
		}
		b.reply(chatID, threadID, fmt.Sprintf("Default runner: %s\nAvailable:\n%s", current, strings.Join(available, "\n")))
		return
	}

	r, err := runner.Get(arg)
	if err != nil {
		b.reply(chatID, threadID, fmt.Sprintf("Unknown runner %q. Available: claude, opencode", arg))
		return
	}

	if !r.DetectInstallation() {
		b.reply(chatID, threadID, fmt.Sprintf("Warning: %q is not installed on this system. Setting anyway.", arg))
	}

	b.SetDefaultRunner(r)
	b.reply(chatID, threadID, fmt.Sprintf("Default runner switched to: %s", r.Name()))
}

// resolveAgentID resolves a partial agent ID to a full ID.
func resolveAgentID(pool *pgxpool.Pool, partialID string) (string, error) {
	agents, err := db.ListAgents(pool)
	if err != nil {
		return "", err
	}

	// Exact match
	for _, a := range agents {
		if a.ID == partialID {
			return a.ID, nil
		}
	}

	// Prefix match
	var matches []string
	for _, a := range agents {
		if strings.HasPrefix(a.ID, partialID) {
			matches = append(matches, a.ID)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no agent matching '%s'", partialID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous ID '%s': matches %s", partialID, strings.Join(matches, ", "))
	}
}
