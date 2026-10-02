// Review dispatch (EX-03, ADR-0005): pipeline tasks completing their worker
// enter 'review' (done-path branch in db.MarkDone). This loop
//
//  1. spawns a fresh, zero-author reviewer agent per review task (soul
//     template pipeline-reviewer, runner/model resolved from the template's
//     frozen extras contract),
//  2. heals a missing review prompt (crash between spawn and enqueue),
//  3. parses the reviewer's outbox for the contract verdict line and
//     transitions the task,
//  4. watchdogs stalled reviews into Needs Human.
//
// The board mirror is purely derived (sync.go DerivedState) — dispatch never
// talks to the ticket system and holds no provider dependency.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// Verdict vocabulary — frozen contract (role-souls AC 8, arch/pipeline.md).
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request_changes"
	VerdictNeedsHuman     = "needs_human"
)

// ReviewerSoulTemplate is the soul template dispatch clones per reviewer
// (migration 035). Dispatch reads its extras and never writes templates.
const ReviewerSoulTemplate = "pipeline-reviewer"

// reviewerRole is the agents.role value for dispatched reviewers.
const reviewerRole = "reviewer"

var (
	verdictRe    = regexp.MustCompile(`(?m)^[ \t]*VERDICT:[ \t]*(approve|request_changes|needs_human)[ \t]*$`)
	verdictAnyRe = regexp.MustCompile(`(?m)^[ \t]*VERDICT:`)
)

// ParseVerdict extracts the contract verdict line from reviewer output.
// The reviewer soul ends its session with exactly one such line; matching is
// line-anchored with the exact three-value vocabulary. Returns ok=false when
// no well-formed verdict line is present.
func ParseVerdict(text string) (string, bool) {
	m := verdictRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// HasMalformedVerdictLine reports contract-violating output: a line prefixed
// VERDICT: that the strict parser rejects (wrong case, unknown value, missing
// space). Logged loudly by the verdict pass; never transitions a task.
func HasMalformedVerdictLine(text string) bool {
	return verdictAnyRe.MatchString(text) && !verdictRe.MatchString(text)
}

// DispatchConfig carries the dispatch loop's knobs.
type DispatchConfig struct {
	// Interval between passes (default 10s, same cadence as sync).
	Interval time.Duration
	// ReviewTimeout is the stall watchdog bound: a live reviewer with no
	// outbox activity for this long parks the task in pending_approval
	// (default 2h, MAQUINISTA_REVIEW_TIMEOUT).
	ReviewTimeout time.Duration
	// SessionName is the tmux session reviewers run in (window cleanup).
	SessionName string
}

// DefaultDispatchConfig returns the documented defaults.
func DefaultDispatchConfig(sessionName string) DispatchConfig {
	return DispatchConfig{Interval: 10 * time.Second, ReviewTimeout: 2 * time.Hour, SessionName: sessionName}
}

// DispatchConfigFromEnv applies MAQUINISTA_REVIEW_TIMEOUT over the defaults.
func DispatchConfigFromEnv(sessionName string) DispatchConfig {
	cfg := DefaultDispatchConfig(sessionName)
	if v := strings.TrimSpace(os.Getenv("MAQUINISTA_REVIEW_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.ReviewTimeout = d
		} else {
			log.Printf("pipeline: dispatch: invalid MAQUINISTA_REVIEW_TIMEOUT %q, using %s", v, cfg.ReviewTimeout)
		}
	}
	return cfg
}

// ReviewSpawnParams carries what the spawner needs to materialize one
// reviewer pane. AgentID is pre-minted by dispatch (fresh, zero-author).
type ReviewSpawnParams struct {
	AgentID      string
	TaskID       string
	WorktreePath string
	RunnerType   string // extras default_runner override ("" = cfg default)
	Model        string // resolved from reasoning_class ("" = runner default)
}

// ReviewSpawner materializes a reviewer agent: agents row (task-bound,
// role reviewer), soul clone, tmux pane, sidecar inbox goroutine.
// Implementations wrap agentspawn.SpawnFresh.
type ReviewSpawner interface {
	SpawnReviewer(ctx context.Context, p ReviewSpawnParams) error
}

// ReviewSpawnerFunc adapts a function to ReviewSpawner.
type ReviewSpawnerFunc func(ctx context.Context, p ReviewSpawnParams) error

// SpawnReviewer implements ReviewSpawner.
func (f ReviewSpawnerFunc) SpawnReviewer(ctx context.Context, p ReviewSpawnParams) error {
	return f(ctx, p)
}

// RunDispatch runs the dispatch loop until ctx is cancelled. killWindow is
// best-effort reviewer pane cleanup (tmux.KillWindow-shaped); nil skips it.
func RunDispatch(ctx context.Context, pool *pgxpool.Pool, cfg DispatchConfig, spawn ReviewSpawner, killWindow func(session, windowID string) error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.ReviewTimeout <= 0 {
		cfg.ReviewTimeout = 2 * time.Hour
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := dispatchPass(ctx, pool, spawn); err != nil {
			log.Printf("pipeline: dispatch: spawn pass: %v", err)
		}
		if err := promptPass(ctx, pool); err != nil {
			log.Printf("pipeline: dispatch: prompt pass: %v", err)
		}
		if err := verdictPass(ctx, pool, cfg.SessionName, killWindow); err != nil {
			log.Printf("pipeline: dispatch: verdict pass: %v", err)
		}
		if err := watchdogPass(ctx, pool, cfg.ReviewTimeout, cfg.SessionName, killWindow); err != nil {
			log.Printf("pipeline: dispatch: watchdog pass: %v", err)
		}
	}
}

// reviewCandidate is a pipeline task in 'review' without a live reviewer.
const reviewCandidatesSQL = `
SELECT t.id, t.worktree_path
FROM tasks t
WHERE t.status = 'review'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL
  AND t.worktree_path IS NOT NULL AND t.worktree_path <> ''
  AND NOT EXISTS (
        SELECT 1 FROM agents a
        WHERE a.task_id = t.id AND a.status <> 'dead' AND a.role = '` + reviewerRole + `')`

func dispatchPass(ctx context.Context, pool *pgxpool.Pool, spawn ReviewSpawner) error {
	rows, err := pool.Query(ctx, reviewCandidatesSQL)
	if err != nil {
		return err
	}
	type cand struct{ taskID, worktree string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.taskID, &c.worktree); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range cands {
		author, err := taskAuthor(ctx, pool, c.taskID)
		if err != nil {
			log.Printf("pipeline: dispatch: author lookup %s: %v", c.taskID, err)
		}
		agentID, err := mintReviewerID(ctx, pool, c.taskID)
		if err != nil {
			log.Printf("pipeline: dispatch: mint %s: %v", c.taskID, err)
			continue
		}
		// Zero-author guard (fresh-mint makes this structural; the explicit
		// check catches identity pathologies). Violation → Needs Human.
		if agentID == author {
			log.Printf("pipeline: dispatch: zero-author violation on %s (reviewer %s == author) — parking needs_human", c.taskID, agentID)
			_, _ = pool.Exec(ctx, `UPDATE tasks SET status='pending_approval' WHERE id=$1 AND status='review'`, c.taskID)
			continue
		}
		runnerType, model, err := resolveTemplateExec(ctx, pool)
		if err != nil {
			log.Printf("pipeline: dispatch: resolve exec for %s: %v", c.taskID, err)
		}
		err = spawn.SpawnReviewer(ctx, ReviewSpawnParams{
			AgentID:      agentID,
			TaskID:       c.taskID,
			WorktreePath: c.worktree,
			RunnerType:   runnerType,
			Model:        model,
		})
		if err != nil {
			// Includes the cross-process unique-live loss; next tick no-ops.
			log.Printf("pipeline: dispatch: spawn reviewer %s for %s: %v", agentID, c.taskID, err)
			continue
		}
		if err := recordReviewRound(ctx, pool, agentID, c.taskID); err != nil {
			log.Printf("pipeline: dispatch: record round %s: %v", c.taskID, err)
		}
		log.Printf("pipeline: dispatch: spawned reviewer %s for task %s (worktree %s)", agentID, c.taskID, c.worktree)
	}
	return nil
}

// taskAuthor returns the agent that recorded the task's latest result —
// the worker whose output is under review.
func taskAuthor(ctx context.Context, pool *pgxpool.Pool, taskID string) (string, error) {
	var author *string
	err := pool.QueryRow(ctx, `
		SELECT agent_id FROM task_context
		WHERE task_id = $1 AND kind = 'result'
		ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&author)
	if err != nil {
		return "", err
	}
	if author == nil {
		return "", nil
	}
	return *author, nil
}

// mintReviewerID picks the first reviewer-<taskID>[-rN] id not present in
// agents (same shape as orchestrator.mintAgentID, kept local to avoid the
// cross-package dependency).
func mintReviewerID(ctx context.Context, pool *pgxpool.Pool, taskID string) (string, error) {
	base := reviewerRole + "-" + taskID
	for n := 1; n < 100; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-r%d", base, n)
		}
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE id=$1)`, candidate).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("mintReviewerID: exhausted retries for %s", base)
}

// resolveTemplateExec reads the reviewer template's frozen extras contract
// and resolves the (runner, model) pair. jsonb scans as []byte (pgx v5
// returns map[string]any for `any` targets — the role-souls gotcha).
// runnerOverride/model may be empty: empty runner = cfg default, empty model
// = the runner's own resolution chain (MAQUINISTA_PI_MODEL env or default).
func resolveTemplateExec(ctx context.Context, pool *pgxpool.Pool) (runnerOverride, model string, err error) {
	var raw []byte
	err = pool.QueryRow(ctx, `SELECT extras FROM soul_templates WHERE id = $1`, ReviewerSoulTemplate).Scan(&raw)
	if err != nil {
		return "", "", fmt.Errorf("load template %s: %w", ReviewerSoulTemplate, err)
	}
	var ex struct {
		DefaultRunner  string `json:"default_runner"`
		ReasoningClass string `json:"reasoning_class"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		return "", "", fmt.Errorf("parse extras of %s: %w", ReviewerSoulTemplate, err)
	}
	modelHigh := strings.TrimSpace(os.Getenv("MAQUINISTA_PI_MODEL_HIGH"))
	modelStd := strings.TrimSpace(os.Getenv("MAQUINISTA_PI_MODEL"))
	_, model = ResolveExec("", ex.DefaultRunner, ex.ReasoningClass, modelHigh, modelStd)
	return ex.DefaultRunner, model, nil
}

// ResolveExec maps the frozen dispatch hints to (runner, model). Pure.
//   - runner: extras default_runner, fallback cfgRunner
//   - model: class "high" → modelHigh, else modelStd (empty falls back to
//     the runner's own chain)
func ResolveExec(cfgRunner, extrasRunner, reasoningClass, modelHigh, modelStd string) (runner, model string) {
	runner = strings.TrimSpace(extrasRunner)
	if runner == "" {
		runner = cfgRunner
	}
	if reasoningClass == "high" && modelHigh != "" {
		return runner, modelHigh
	}
	return runner, modelStd
}

// recordReviewRound increments review_rounds and enqueues the review prompt
// in ONE tx. The inbox insert is dedup'd by EnqueueInbox's
// (origin_channel, external_msg_id) conflict target, so a crash between
// spawn and enqueue heals on the next tick without duplicating.
func recordReviewRound(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var round int
	if err := tx.QueryRow(ctx, `
		UPDATE tasks SET review_rounds = review_rounds + 1
		WHERE id = $1
		RETURNING review_rounds
	`, taskID).Scan(&round); err != nil {
		return fmt.Errorf("bump review_rounds: %w", err)
	}

	content, err := json.Marshal(map[string]any{
		"type":    "review",
		"task_id": taskID,
		"round":   round,
		"prompt":  reviewPromptBody(taskID, round),
	})
	if err != nil {
		return err
	}
	_, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: fmt.Sprintf("review:%s:%d", taskID, round),
		Content:       content,
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// reviewPromptBody is the per-round task briefing. The reviewer soul carries
// the full method + verdict contract; this carries the round specifics.
func reviewPromptBody(taskID string, round int) string {
	return fmt.Sprintf(
		"Review round %d for task %s. The implementation is committed in your cwd (the task worktree). "+
			"Run `git fetch origin && git diff origin/main...HEAD` (plus `git log origin/main..HEAD`) to see the change, "+
			"read the spec under .specs/ if present, run the validators the spec names, and judge the change on its merits. "+
			"End your reply with exactly one line: VERDICT: approve | VERDICT: request_changes | VERDICT: needs_human.",
		round, taskID)
}

// promptPass heals the crash-between-spawn-and-enqueue case: a live reviewer
// whose round's prompt row is missing gets exactly one (dedup'd) re-enqueue.
const promptHealSQL = `
SELECT a.id, t.id, t.review_rounds
FROM tasks t
JOIN agents a ON a.task_id = t.id AND a.status <> 'dead' AND a.role = '` + reviewerRole + `'
WHERE t.status = 'review'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL
  AND NOT EXISTS (
        SELECT 1 FROM agent_inbox i
        WHERE i.agent_id = a.id
          AND i.origin_channel = 'task'
          AND i.external_msg_id = 'review:' || t.id || ':' || t.review_rounds)`

func promptPass(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, promptHealSQL)
	if err != nil {
		return err
	}
	type gap struct{ agentID, taskID string; round int }
	var gaps []gap
	for rows.Next() {
		var g gap
		if err := rows.Scan(&g.agentID, &g.taskID, &g.round); err != nil {
			rows.Close()
			return err
		}
		gaps = append(gaps, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range gaps {
		log.Printf("pipeline: dispatch: healing missing review prompt %s round %d", g.taskID, g.round)
		if err := enqueueReviewPrompt(ctx, pool, g.agentID, g.taskID, g.round); err != nil {
			log.Printf("pipeline: dispatch: heal prompt %s: %v", g.taskID, err)
		}
	}
	return nil
}

func enqueueReviewPrompt(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, round int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	content, err := json.Marshal(map[string]any{
		"type": "review", "task_id": taskID, "round": round,
		"prompt": reviewPromptBody(taskID, round),
	})
	if err != nil {
		return err
	}
	_, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: fmt.Sprintf("review:%s:%d", taskID, round),
		Content:       content,
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// liveReviewersSQL drives both the verdict pass and the watchdog: live
// reviewer agents on pipeline tasks still in 'review'.
const liveReviewersSQL = `
SELECT a.id, a.tmux_session, a.tmux_window, t.id
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE a.role = '` + reviewerRole + `'
  AND a.status <> 'dead'
  AND t.status = 'review'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL`

func verdictPass(ctx context.Context, pool *pgxpool.Pool, sessionName string, killWindow func(session, windowID string) error) error {
	var reviewers []liveReviewer
	if err := scanReviewers(ctx, pool, liveReviewersSQL, nil, &reviewers); err != nil {
		return err
	}
	for _, r := range reviewers {
		verdict, found, malformed := latestVerdict(ctx, pool, r.agentID)
		if malformed {
			log.Printf("pipeline: dispatch: reviewer %s emitted a malformed VERDICT line (contract violation) — waiting for a well-formed one", r.agentID)
		}
		if !found {
			continue
		}
		status, ok := statusForVerdict(verdict)
		if !ok {
			continue
		}
		retire, err := applyVerdict(ctx, pool, r.agentID, r.taskID, verdict, status)
		if err != nil {
			log.Printf("pipeline: dispatch: apply verdict %s → %s: %v", r.taskID, verdict, err)
			continue
		}
		if !retire {
			continue // task row raced to another status; leave the pane be
		}
		log.Printf("pipeline: dispatch: verdict %s on %s (reviewer %s) → %s", verdict, r.taskID, r.agentID, status)
		killReviewerPane(sessionName, r.session, r.window, killWindow)
	}
	return nil
}

// latestVerdict scans the reviewer's newest assistant outbox rows, newest
// first; the first well-formed verdict wins. malformed reports a
// contract-violating VERDICT line in the newest rows (log-only signal).
func latestVerdict(ctx context.Context, pool *pgxpool.Pool, agentID string) (verdict string, found, malformed bool) {
	rows, err := pool.Query(ctx, `
		SELECT content->>'text' FROM agent_outbox
		WHERE agent_id = $1 AND content ? 'text'
		ORDER BY created_at DESC LIMIT 10
	`, agentID)
	if err != nil {
		return "", false, false
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return "", false, false
		}
		if v, ok := ParseVerdict(text); ok {
			return v, true, malformed
		}
		if HasMalformedVerdictLine(text) {
			malformed = true
		}
	}
	return "", false, malformed
}

// statusForVerdict maps the frozen vocabulary onto task statuses.
func statusForVerdict(v string) (string, bool) {
	switch v {
	case VerdictApprove:
		return "ready_to_merge", true
	case VerdictRequestChanges:
		return "changes_requested", true
	case VerdictNeedsHuman:
		return "pending_approval", true
	default:
		return "", false
	}
}

// applyVerdict transitions the task, records the verdict, and retires the
// reviewer — one tx, guarded on the task still being in 'review'. Returns
// false when the row raced (nothing written).
func applyVerdict(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, verdict, status string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE tasks SET status=$1 WHERE id=$2 AND status='review'`, status, taskID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'verdict', $3)
	`, taskID, agentID, "VERDICT: "+verdict); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1`, agentID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// watchdogPass parks stalled reviews: a live reviewer with NO outbox
// activity (the monitor writes rows as the agent streams) for longer than
// the timeout flips the task to pending_approval and retires the pane.
// liveReviewer is one live reviewer pane on a pipeline review task.
type liveReviewer struct {
	agentID, session, window, taskID string
}

func watchdogPass(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, sessionName string, killWindow func(session, windowID string) error) error {
	var reviewers []liveReviewer
	if err := scanReviewers(ctx, pool, liveReviewersSQL+`
	  AND NOT EXISTS (
	        SELECT 1 FROM agent_outbox o
	        WHERE o.agent_id = a.id AND o.created_at > NOW() - make_interval(secs => $1))`,
		[]any{timeout.Seconds()}, &reviewers); err != nil {
		return err
	}
	for _, r := range reviewers {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE tasks SET status='pending_approval' WHERE id=$1 AND status='review'`, r.taskID)
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		if tag.RowsAffected() == 0 {
			tx.Rollback(ctx)
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_context (task_id, agent_id, kind, content)
			VALUES ($1, $2, 'verdict', $3)
		`, r.taskID, r.agentID, fmt.Sprintf("watchdog: review stalled past %s — needs human", timeout)); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1`, r.agentID); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		log.Printf("pipeline: dispatch: watchdog retired stalled reviewer %s on %s → needs_human", r.agentID, r.taskID)
		killReviewerPane(sessionName, r.session, r.window, killWindow)
	}
	return nil
}

func scanReviewers(ctx context.Context, pool *pgxpool.Pool, sql string, args []any, out *[]liveReviewer) error {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r liveReviewer
		if err := rows.Scan(&r.agentID, &r.session, &r.window, &r.taskID); err != nil {
			return err
		}
		*out = append(*out, r)
	}
	return rows.Err()
}

func killReviewerPane(sessionName, session, window string, killWindow func(session, windowID string) error) {
	if killWindow == nil {
		return
	}
	s := session
	if s == "" {
		s = sessionName
	}
	if window == "" {
		return
	}
	if err := killWindow(s, window); err != nil {
		log.Printf("pipeline: dispatch: kill window %s:%s: %v (continuing)", s, window, err)
	}
}
