package bot

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/routing"
	"github.com/maquinista-labs/maquinista/internal/tmux"
)

// handleTextMessage resolves the target agent via the §8.1 ladder and
// routes the text through agent_inbox.
func (b *Bot) handleTextMessage(msg *tgbotapi.Message) {
	userID := strconv.FormatInt(msg.From.ID, 10)
	threadID := strconv.Itoa(getThreadID(msg))
	chatID := msg.Chat.ID

	if b.handleAddTaskReply(msg) {
		return
	}
	if b.handlePendingInput(msg) {
		return
	}

	cancelBashCapture(msg.From.ID, getThreadID(msg))

	b.state.SetGroupChatID(userID, threadID, chatID)
	b.saveState()

	text := msg.Text

	// ! prefix still targets the existing window binding (bash mode is a
	// pty-level concern and doesn't participate in the routing ladder).
	if strings.HasPrefix(text, "!") && len(text) > 1 {
		if windowID, bound := b.state.GetWindowForThread(userID, threadID); bound {
			b.handleBashCommand(msg, windowID, text)
			return
		}
		b.reply(chatID, getThreadID(msg), "No window bound for ! commands. Send a regular message first.")
		return
	}

	pool := b.getPool()
	if pool == nil {
		b.reply(chatID, getThreadID(msg), "Error: agent mailbox unavailable (DATABASE_URL).")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	chatIDCopy := chatID
	res, err := routing.Resolve(ctx, pool, userID, threadID, &chatIDCopy, text)
	if errors.Is(err, routing.ErrRequirePicker) {
		b.showAgentPicker(chatID, getThreadID(msg), msg.From.ID, text)
		return
	}
	if err != nil {
		log.Printf("routing.Resolve: %v", err)
		b.reply(chatID, getThreadID(msg), "Error: routing failed. Check server logs.")
		return
	}

	// Verify the resolved agent actually exists in agents before handing
	// it to the inbox — prevents FK violations if a mention references a
	// bogus id.
	if !b.agentExists(ctx, pool, res.AgentID) {
		b.reply(chatID, getThreadID(msg),
			"Unknown agent @"+res.AgentID+". Start it with `AGENT_ID="+res.AgentID+" claude` or pick another.")
		return
	}

	// Keep state.json bindings in sync with topic_agent_bindings so the
	// monitor (which still reads state.*) can route responses back. See
	// plans/json-state-migration.md Phase B for the proper fix that
	// drops this dual-write.
	b.syncAgentStateFor(ctx, pool, res.AgentID, userID, threadID, chatID)

	if !b.routeTextViaInbox(msg, res.AgentID, res.Text) {
		log.Printf("mailbox.inbound: routing failed for %s", res.AgentID)
		b.reply(chatID, getThreadID(msg), "Error: agent mailbox write failed. Check DATABASE_URL.")
	}
}

// syncAgentStateFor populates the in-memory state.json maps the legacy
// monitor relies on (thread→window, user+thread→chat, window→runner)
// from the agents + bindings tables. Swallows errors — a stale
// state.json doesn't break routing, it just means responses don't
// reach Telegram.
func (b *Bot) syncAgentStateFor(ctx context.Context, pool *pgxpool.Pool, agentID, userID, threadID string, chatID int64) {
	if pool == nil || agentID == "" {
		return
	}
	var window, runner string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(tmux_window,''), COALESCE(runner_type,'')
		FROM agents WHERE id=$1
	`, agentID).Scan(&window, &runner)
	if err != nil || window == "" {
		return
	}
	b.state.BindThread(userID, threadID, window)
	b.state.SetGroupChatID(userID, threadID, chatID)
	if runner != "" {
		b.state.SetWindowRunner(window, runner)
	}
	b.state.SetWindowDisplayName(window, agentID)
	b.saveState()
}

// agentExists returns true if agent_id is present in agents. Fail-open on
// query error so a DB blip doesn't false-reject a legitimate message.
func (b *Bot) agentExists(ctx context.Context, pool *pgxpool.Pool, agentID string) bool {
	var one int
	err := pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE id = $1`, agentID).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		log.Printf("agentExists(%s): %v", agentID, err)
		return true
	}
	return true
}

// handleBashCommand sends a ! command to Claude's bash mode.
func (b *Bot) handleBashCommand(msg *tgbotapi.Message, windowID, text string) {
	session := b.config.TmuxSessionName

	// Send ! first to enter bash mode
	if err := tmux.SendKeys(session, windowID, "!"); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, text)
			return
		}
		log.Printf("Error sending ! to %s: %v", windowID, err)
		return
	}

	// Wait 1 second
	time.Sleep(1 * time.Second)

	// Send the rest of the command (without !) + Enter
	cmd := text[1:]
	if err := tmux.SendKeysWithDelay(session, windowID, cmd, 500); err != nil {
		if tmux.IsWindowDead(err) {
			b.handleDeadWindow(msg, windowID, text)
			return
		}
		log.Printf("Error sending bash command to %s: %v", windowID, err)
		return
	}

	// Launch capture goroutine
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)
	b.startBashCapture(msg.From.ID, chatID, threadID, windowID, cmd)
}

// routeCallback routes callback queries to the appropriate handler.
func (b *Bot) routeCallback(cq *tgbotapi.CallbackQuery) {
	data := cq.Data

	// Answer callback to dismiss spinner
	callback := tgbotapi.NewCallback(cq.ID, "")
	b.api.Request(callback)

	switch {
	case strings.HasPrefix(data, "apick_"):
		b.processAgentPickerCallback(cq)
	case strings.HasPrefix(data, "win_"):
		b.processWindowCallback(cq)
	case strings.HasPrefix(data, "hist_"):
		b.handleHistoryCallback(cq)
	case strings.HasPrefix(data, "ss_"):
		b.handleScreenshotCallback(cq)
	case strings.HasPrefix(data, "nav_"):
		b.handleInteractiveCallback(cq)
	case strings.HasPrefix(data, "get_"):
		b.processFileBrowserCallback(cq)
	case strings.HasPrefix(data, "task_"):
		b.processAddTaskCallback(cq)
	case strings.HasPrefix(data, "tpick_"):
		b.processTaskPickerCallback(cq)
	case strings.HasPrefix(data, "merge_"):
		b.handleMergeCallback(cq)
	case strings.HasPrefix(data, "plan_"):
		b.processPlanCallback(cq)
	case strings.HasPrefix(data, "planner_"):
		b.processPlannerCallback(cq, data)
	case strings.HasPrefix(data, "approval_"):
		b.processApprovalCallback(cq)
	case strings.HasPrefix(data, "agent_"):
		b.processAgentCallback(cq, data)
	case strings.HasPrefix(data, "menu_"):
		b.handleMenuCallback(cq)
	case data == "noop":
		// No-op button (e.g., page counter), already answered above
	default:
		log.Printf("Unknown callback data: %s", data)
	}
}

// handleHistoryCallback handles history pagination callbacks.
func (b *Bot) handleHistoryCallback(cq *tgbotapi.CallbackQuery) {
	b.handleHistoryCB(cq)
}

// handleScreenshotCallback handles screenshot control callbacks.
func (b *Bot) handleScreenshotCallback(cq *tgbotapi.CallbackQuery) {
	b.handleScreenshotCB(cq)
}

// handleInteractiveCallback is implemented in interactive.go
