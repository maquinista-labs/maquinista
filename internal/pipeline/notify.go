package pipeline

// Telegram plumbing (ADR-0005 EX-06). Pipeline events reach Telegram through
// the stock delivery path — agent_outbox → relay binding leg →
// channel_deliveries → dispatcher — by writing outbox rows for the synthetic
// 'pipeline' agent seeded by migration 036. The bot's topic provisioner
// gives that agent its "Pipeline" forum topic + owner binding; this file
// only produces the content the relay already knows how to route.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// NotifyAgentID is the synthetic agent whose outbox rows carry pipeline
// events to the Pipeline topic. Must match migration 036 (and the bot's
// ensurePipelineTopic binding), which is why it lives here and not in bot.
const NotifyAgentID = "pipeline"

// Notify commits one pipeline notification as an outbox row of the synthetic
// pipeline agent. The relay fans it out to the agent's owner binding — the
// Pipeline topic — with the standard dedup/retry machinery; nothing here
// knows about Telegram addressing.
func Notify(ctx context.Context, pool *pgxpool.Pool, text string) error {
	content, err := json.Marshal(map[string]string{"type": "text", "text": text})
	if err != nil {
		return fmt.Errorf("pipeline: notify encode: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pipeline: notify begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := mailbox.AppendOutbox(ctx, tx, mailbox.OutboxMessage{
		AgentID: NotifyAgentID,
		Content: content,
	}); err != nil {
		return fmt.Errorf("pipeline: notify append: %w", err)
	}
	return tx.Commit(ctx)
}

// Notifyf is the exported fire-and-forget form used inside guarded
// transition arms — including ones that live outside this package
// (MAQ-22: the task scheduler's implementor claim / stale-claim requeue
// and tools.SetPRUrl's PR-opened flip): a failed notification is logged,
// never allowed to fail the pipeline transition it reports (the state
// change is the system of record; the note is a courtesy).
func Notifyf(ctx context.Context, pool *pgxpool.Pool, format string, args ...any) {
	if err := Notify(ctx, pool, fmt.Sprintf(format, args...)); err != nil {
		log.Printf("pipeline: notify: %v", err)
	}
}

// notifyf is the in-package spelling of Notifyf.
func notifyf(ctx context.Context, pool *pgxpool.Pool, format string, args ...any) {
	Notifyf(ctx, pool, format, args...)
}

// TaskTitle returns "<title> (<short-id>)" for summaries, tolerating a
// missing row (best-effort label, never a reason to fail the transition).
// Exported for the MAQ-22 lifecycle one-liners emitted outside this
// package (task scheduler claim/requeue, tools.SetPRUrl).
func TaskTitle(ctx context.Context, pool *pgxpool.Pool, taskID string) string {
	var title string
	if err := pool.QueryRow(ctx,
		`SELECT title FROM tasks WHERE id = $1`, taskID).Scan(&title); err != nil {
		return taskID
	}
	return fmt.Sprintf("%s (%s)", title, taskID)
}

// prLinkSuffix returns "\n🔗 PR: <url>" when the task has a pr_url, else "".
// Best-effort (a lookup failure logs and yields no link) so tasks without a
// PR — or an unreadable row — degrade to the old linkless message instead of
// surfacing a null/empty link (MAQ-10).
func prLinkSuffix(ctx context.Context, pool *pgxpool.Pool, taskID string) string {
	var prURL *string
	if err := pool.QueryRow(ctx, `SELECT pr_url FROM tasks WHERE id = $1`, taskID).Scan(&prURL); err != nil {
		log.Printf("pipeline: notify: pr_url lookup %s: %v", taskID, err)
		return ""
	}
	if prURL == nil || *prURL == "" {
		return ""
	}
	return "\n🔗 PR: " + *prURL
}

// notifyVerdict turns an applied review verdict into the Pipeline-topic
// summary: approve is the merge proposal, request_changes notes the fixer
// round, needs_human / round-cap are the questions. A task with a PR gets
// the link on every verdict (MAQ-10). Verdicts landed before EX-06 never
// re-notify: the emission sits inside the guarded transition.
func notifyVerdict(ctx context.Context, pool *pgxpool.Pool, taskID, title, verdict, landed string, round, maxRounds int) {
	label := title
	if label == "" {
		label = TaskTitle(ctx, pool, taskID)
	}
	pr := prLinkSuffix(ctx, pool, taskID)
	switch {
	case landed == "pending_approval":
		notifyf(ctx, pool, "🆘 %s: review round cap %d reached (%s) — parked needs-human. Decide with `maquinista approve %s` / `maquinista reject %s`.%s",
			label, maxRounds, verdict, taskID, taskID, pr)
	case verdict == VerdictApprove:
		// MAQ-11: the proposal teaches the comment verbs (short id — typeable
		// from a phone) instead of the CLI-only form. MAQ-10: the PR link
		// rides along on every verdict.
		notifyf(ctx, pool, "✅ %s approved (review round %d) → ready_to_merge. Merge proposal: reply `approve %s` here or comment `approve` on the ticket issue — or set PIPELINE_AUTO_MERGE=1 for autonomous merges.%s",
			label, round, shortTaskID(taskID), pr)
	case verdict == VerdictRequestChanges:
		notifyf(ctx, pool, "🔁 %s: request_changes (review round %d) — fixer spawning.%s",
			label, round, pr)
	default: // needs_human
		notifyf(ctx, pool, "🆘 %s: reviewer escalated needs-human. Decide with `maquinista approve %s` / `maquinista reject %s`.%s",
			label, taskID, taskID, pr)
	}
}
