package bot

// Pipeline comment verbs (MAQ-11): an allowed user approves a
// ready_to_merge task from a surface he already has open. Telegram form:
// `/approve <task-ref>` as a bot command, or a bare `approve <task-ref>`
// typed in the Pipeline topic (the short id from the merge-proposal note;
// a full uuid resolves too). Both route into pipeline.ApproveRef — the same
// arm the CLI uses — so the merge rides the standard queue flow and the
// synthetic pipeline agent never needs a sidecar (without the intercept, an
// in-topic `approve …` would strand in its inbox forever).
//
// Authorization is the stock isAuthorized gate (ALLOWED_USERS) applied in
// handleUpdate; nothing here re-checks and nothing accepts approvals from
// anyone else.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
)

// ApproveFunc runs the shared approve verb for a task reference (full uuid
// or prefix); actor is the display identity recorded in the task
// observation. ran=false with a nil error is the documented no-op (task not
// ready_to_merge, or merge mode is not gh). Injected by cmd_start, which
// owns the provider + gh wiring.
type ApproveFunc func(ctx context.Context, taskRef, actor string) (ran bool, status string, err error)

// SetApproveFunc injects the approve backend. Call before Run; nil (never
// set) makes the verb reply "not wired" instead of acting.
func (b *Bot) SetApproveFunc(fn ApproveFunc) {
	b.approveFn = fn
}

// approveTextRe matches the in-topic verb: `approve <task-ref>`. Case
// insensitive, optional slash, one reference token.
var approveTextRe = regexp.MustCompile(`(?i)^/?approve\s+(\S+)$`)

// handlePipelineApproveText intercepts `approve <ref>` typed into the
// Pipeline topic. Returns false when the text isn't the verb or the thread
// isn't the Pipeline topic — the caller falls through to the normal routing
// ladder (the verb must never hijack a regular agent conversation).
func (b *Bot) handlePipelineApproveText(msg *tgbotapi.Message) bool {
	if msg == nil || strings.TrimSpace(msg.Text) == "" {
		return false
	}
	m := approveTextRe.FindStringSubmatch(strings.TrimSpace(msg.Text))
	if m == nil {
		return false
	}
	if !b.isPipelineTopic(b.getPool(), strconv.Itoa(getThreadID(msg))) {
		return false
	}
	b.runApprove(msg, m[1])
	return true
}

// isPipelineTopic reports whether threadID carries an owner binding for the
// synthetic pipeline notifier agent. Fails closed (false) on errors — a DB
// blip must not hijack the routing ladder.
func (b *Bot) isPipelineTopic(pool *pgxpool.Pool, threadID string) bool {
	if pool == nil || threadID == "" || threadID == "0" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var one int
	err := pool.QueryRow(ctx, `
		SELECT 1 FROM topic_agent_bindings
		WHERE agent_id = $1 AND thread_id = $2 AND binding_type = 'owner'
		LIMIT 1
	`, pipeline.NotifyAgentID, threadID).Scan(&one)
	return err == nil
}

// handleApproveCommand is the /approve form: explicit, allowed-user gated,
// usable from any topic.
func (b *Bot) handleApproveCommand(msg *tgbotapi.Message) {
	taskRef := strings.TrimSpace(msg.CommandArguments())
	if taskRef == "" {
		b.reply(msg.Chat.ID, getThreadID(msg), "Usage: /approve <task-id-prefix>")
		return
	}
	b.runApprove(msg, taskRef)
}

// runApprove acks immediately and runs the merge async — the rebase + CI
// gate can take minutes and the update loop must not stall. The merge
// outcome reaches this topic through the standard pipeline notifier; the
// async part only reports the no-op and error cases the notifier can't see.
func (b *Bot) runApprove(msg *tgbotapi.Message, taskRef string) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	if b.approveFn == nil {
		b.reply(chatID, threadID, "Approve is not wired (no merge backend in this process).")
		return
	}

	actor := actorName(msg.From)
	b.reply(chatID, threadID, fmt.Sprintf("🍴 Approve received for %s — running the merge flow…", taskRef))

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ran, status, err := b.approveFn(ctx, taskRef, actor)
		if err != nil {
			log.Printf("approve %s (by %s): %v", taskRef, actor, err)
			b.reply(chatID, threadID, fmt.Sprintf("⚠️ Approve for %s failed: %v", taskRef, err))
			return
		}
		if !ran {
			b.reply(chatID, threadID,
				fmt.Sprintf("ℹ️ %s is in status %q — approve is a no-op (only ready_to_merge tasks merge on approve).", taskRef, status))
			return
		}
		// Success: the merge flow's own notifyf lands here (merged /
		// conflict / failed note) — no duplicate ack.
	}()
}

// actorName renders a Telegram user as an audit identity: @username, else
// the display name, else the numeric id.
func actorName(u *tgbotapi.User) string {
	if u == nil {
		return "unknown"
	}
	if u.UserName != "" {
		return "@" + u.UserName
	}
	if name := strings.TrimSpace(u.FirstName + " " + u.LastName); name != "" {
		return name
	}
	return strconv.FormatInt(u.ID, 10)
}
