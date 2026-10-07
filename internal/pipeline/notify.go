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
	"strings"
	"time"

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
	return NotifyTask(ctx, pool, "", text)
}

// NotifyTask is Notify with the task reference stamped into the outbox
// content ("task_id" key). The extra key is inert for every consumer — the
// dispatcher renders "text" only, the dashboard reads it too — but it is
// what makes a notification reply-commentable (MAQ-24): the bot resolves a
// replied-to Telegram message back to the task via this key alone, never by
// parsing notification prose.
func NotifyTask(ctx context.Context, pool *pgxpool.Pool, taskID, text string) error {
	if taskID != "" {
		text = decorateTaskNote(ctx, pool, taskID, text)
	}
	content, err := json.Marshal(map[string]string{"type": "text", "text": text, "task_id": taskID})
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

// Notifyf is the fire-and-forget form used inside transition arms: a failed
// notification is logged, never allowed to fail the pipeline transition it
// reports (the state change is the system of record; the note is a courtesy).
func notifyf(ctx context.Context, pool *pgxpool.Pool, format string, args ...any) {
	notifyTaskf(ctx, pool, "", format, args...)
}

// notifyTaskf is notifyf for notes about one task: the task_id rides in the
// outbox content so a Telegram reply to the note can be posted as a PR
// comment (MAQ-24). Same fire-and-forget contract.
func notifyTaskf(ctx context.Context, pool *pgxpool.Pool, taskID, format string, args ...any) {
	if err := NotifyTask(ctx, pool, taskID, fmt.Sprintf(format, args...)); err != nil {
		log.Printf("pipeline: notify: %v", err)
	}
}

// Notifyf is the exported fire-and-forget form for packages outside the
// pipeline (taskscheduler park paths, MAQ-13): same contract — a failed
// notification is logged, never fails the caller.
func Notifyf(ctx context.Context, pool *pgxpool.Pool, format string, args ...any) {
	notifyf(ctx, pool, format, args...)
}

// NotifyTaskf is Notifyf for notes about one task, exported for the same
// out-of-package callers: the note rides the task_id so a Telegram reply to
// it lands as a PR comment (MAQ-24).
func NotifyTaskf(ctx context.Context, pool *pgxpool.Pool, taskID, format string, args ...any) {
	notifyTaskf(ctx, pool, taskID, format, args...)
}

// ---- human rendering (MAQ-37) ---------------------------------------------
//
// Notifications are written for the operator, not the machine: the headline
// names the task as every human surface already does — `[MAQ-n] <title>` —
// raw task UUIDs and internal agent ids never appear in prose, statuses get
// a plain-language phrase, and every note carries the Linear issue URL (plus
// the PR URL when the task has one). The machine block (uuid, rounds,
// timings) stays where it belongs: the task_context observation/verdict rows
// each guarded transition already writes.

// taskTitle renders the human headline for a task: `[<issue_key>] <title>`
// with the issue key from ticket_issue_map (the MAQ-n identifier every human
// surface — Linear, PR titles — already uses). Without a mapping it falls
// back to the raw title (bridge intake titles carry the `[MAQ-n]` prefix
// themselves), and to the short id when not even the task row is readable.
// Best-effort: a missing row is never a reason to fail the transition.
func taskTitle(ctx context.Context, pool *pgxpool.Pool, taskID string) string {
	var title, issueKey *string
	if err := pool.QueryRow(ctx, `
		SELECT t.title, m.issue_key
		FROM   tasks t
		LEFT JOIN ticket_issue_map m ON m.task_id = t.id
		WHERE  t.id = $1
	`, taskID).Scan(&title, &issueKey); err != nil || title == nil || *title == "" {
		return shortTaskID(taskID)
	}
	t := strings.TrimSpace(*title)
	if issueKey != nil && *issueKey != "" {
		// The intake title already carries a `[MAQ-n]` prefix; replace it
		// with the canonical issue key instead of doubling the brackets.
		if rest, ok := strings.CutPrefix(t, "["); ok {
			if _, body, found := strings.Cut(rest, "]"); found {
				t = strings.TrimSpace(body)
			}
		}
		return "[" + *issueKey + "] " + t
	}
	return t
}

// TaskTitle is the exported form for out-of-package callers that compose
// their own notify notes (taskscheduler park paths, MAQ-13).
func TaskTitle(ctx context.Context, pool *pgxpool.Pool, taskID string) string {
	return taskTitle(ctx, pool, taskID)
}

// decorateTaskNote is the seam's decoration pass (MAQ-37 AC2): every
// task-scoped note carries the Linear issue URL and — when the task has one
// — the PR URL, appended as link lines. Best-effort: an unreadable task row
// degrades to the undecorated text, never to null/empty links.
func decorateTaskNote(ctx context.Context, pool *pgxpool.Pool, taskID, text string) string {
	var ticketURL, prURL *string
	if err := pool.QueryRow(ctx, `
		SELECT metadata->>'ticket_url', pr_url FROM tasks WHERE id = $1
	`, taskID).Scan(&ticketURL, &prURL); err != nil {
		log.Printf("pipeline: notify: decorate %s: %v", taskID, err)
		return text
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, "\n"))
	if ticketURL != nil && *ticketURL != "" {
		b.WriteString("\n🎫 Issue: " + *ticketURL)
	}
	if prURL != nil && *prURL != "" {
		b.WriteString("\n🔗 PR: " + *prURL)
	}
	return b.String()
}

// prURLof returns the task's pr_url ("" when none or unreadable) for
// callers that build their own link from it (the merge-commit line).
func prURLof(ctx context.Context, pool *pgxpool.Pool, taskID string) string {
	var prURL *string
	if err := pool.QueryRow(ctx, `SELECT pr_url FROM tasks WHERE id = $1`, taskID).Scan(&prURL); err != nil || prURL == nil {
		return ""
	}
	return *prURL
}

// commitLinkSuffix renders the merged-notification's commit line: the PR URL
// shape `https://<host>/<owner>/<repo>/pull/N` becomes a link to the merge
// commit. Unparseable hosts degrade to the bare sha (still identifying, just
// not clickable); no sha at all yields no line.
func commitLinkSuffix(prURL, sha string) string {
	if sha == "" {
		return ""
	}
	if base, _, found := strings.Cut(prURL, "/pull/"); found && strings.HasPrefix(base, "http") {
		return "\n🔨 Merged as " + base + "/commit/" + sha
	}
	return "\n🔨 Commit: " + sha
}

// DurHuman renders a watchdog bound the way prose reads ("~30m", "~1h30"),
// not the way Go prints it ("30m0s"). Sub-minute bounds keep the raw shape —
// they only appear with degenerate config. Exported: the taskscheduler's
// freeze arms compose their own human sentences.
func DurHuman(d time.Duration) string {
	switch {
	case d >= time.Hour:
		h := int(d / time.Hour)
		if m := int((d % time.Hour) / time.Minute); m > 0 {
			return fmt.Sprintf("%dh%02dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return d.String()
	}
}

// agentRole extracts the role word from a minted worker id
// `<role>-<taskID>[-rN]` — the taskID is a uuid (no dashes-stripping), so
// the role is the first dash-separated segment. Unknown shapes yield
// "agent".
func agentRole(agentID string) string {
	if role, _, ok := strings.Cut(agentID, "-"); ok && role != "" {
		return role
	}
	return "agent"
}

// agentRound extracts the respawn round suffix (-rN) from a minted worker
// id; 0 when the id carries none (first attempt).
func agentRound(agentID string) int {
	n := strings.LastIndex(agentID, "-r")
	if n < 0 {
		return 0
	}
	var r int
	if _, err := fmt.Sscanf(agentID[n+2:], "%d", &r); err != nil {
		return 0
	}
	return r
}

// roleHuman renders a worker id as prose: "the implementor (round 4)".
// Never the internal id itself (MAQ-37 AC1).
func roleHuman(agentID string) string {
	return RoleHuman(agentRole(agentID), agentID)
}

// RoleHuman renders a claim as prose from the authoritative role word plus
// the minted worker id — only the id's -rN suffix is read: "the implementor
// (round 4)", "the executor". Never the internal id itself (MAQ-37 AC1).
// Exported for out-of-package claim announcers (taskscheduler), where the
// role column is at hand and the id may not carry the role as its prefix.
func RoleHuman(role, agentID string) string {
	if r := agentRound(agentID); r > 0 {
		return fmt.Sprintf("the %s (round %d)", role, r)
	}
	return "the " + role
}

// notifyVerdict turns an applied review verdict into the Pipeline-topic
// summary: approve is the merge proposal, request_changes notes the fixer
// round, needs_human / round-cap are the questions. A task with a PR gets
// the link on every verdict (MAQ-10). Verdicts landed before EX-06 never
// re-notify: the emission sits inside the guarded transition.
func notifyVerdict(ctx context.Context, pool *pgxpool.Pool, taskID, title, verdict, landed string, round, maxRounds int) {
	// MAQ-37: the headline is always the canonical `[MAQ-n] <title>` —
	// the raw title the reviewers query carried is only a fallback for a
	// task row that vanished mid-pass (better a title than a short id).
	label := TaskTitle(ctx, pool, taskID)
	if title != "" && label == shortTaskID(taskID) && len(title) > len(label) {
		label = title
	}
	short := shortTaskID(taskID)
	// MAQ-24: every verdict note carries the task_id in its outbox content —
	// a plain reply to it lands as a PR comment. The Linear issue and PR
	// links ride on every verdict (decoration in NotifyTask); the approve
	// proposal names each approve path next to its clickable link.
	switch {
	case landed == "pending_approval":
		notifyTaskf(ctx, pool, taskID,
			"🆘 %s: review hit its round cap (%d rounds without an approval) — parked for you. Decide: reply `approve` or `reject` here, comment the same on the ticket issue, or run `maquinista approve %s` / `maquinista reject %s`.",
			label, maxRounds, short, short)
	case verdict == VerdictApprove:
		notifyTaskf(ctx, pool, taskID,
			"✅ %s: review approved (round %d) — queued for merge. To merge now: reply `approve` here, or comment `approve` on the ticket issue (link below).",
			label, round)
	case verdict == VerdictRequestChanges:
		notifyTaskf(ctx, pool, taskID,
			"🔁 %s: the reviewer requested changes (round %d) — a fixer is picking it up. No action needed.",
			label, round)
	default: // needs_human
		notifyTaskf(ctx, pool, taskID,
			"🆘 %s: the reviewer escalated — needs your call. Reply `approve` (merge as-is) or `reject` here, or comment the same on the ticket issue (link below).",
			label)
	}
}
