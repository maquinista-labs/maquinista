// Review dispatch (EX-03/EX-04, ADR-0005): pipeline tasks completing their
// worker enter 'review' (done-path branch in db.MarkDone). This loop
//
//  1. spawns a fresh, zero-author reviewer agent per review task (soul
//     template pipeline-reviewer, runner/model resolved from the template's
//     frozen extras contract),
//  2. heals a missing review prompt (crash between spawn and enqueue),
//  3. parses the reviewer's outbox for the contract verdict line and
//     transitions the task,
//  4. spawns a fresh fixer per changes_requested episode (pipeline-fixer
//     soul, same worktree) fed with the reviewer's findings — the fixer
//     completes via maquinista-done, which re-enters 'review' through the
//     done-path branch and closes the loop,
//  5. caps the loop: a request_changes landing at/after the review-round cap
//     parks the task in Needs Human instead of cycling forever,
//  6. watchdogs stalled reviews AND stalled fixes into Needs Human.
//
// The board mirror is purely derived (sync.go DerivedState) — dispatch never
// talks to the ticket system and holds no provider dependency.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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

// FixerSoulTemplate is the soul template dispatch clones per fixer episode
// (migration 035): resolve the reviewer's findings in the SAME worktree/PR.
const FixerSoulTemplate = "pipeline-fixer"

// MergerSoulTemplate is the soul template dispatch (and the comment-command
// resolve verb) clones per merge/resolve session (migration 035): rebase,
// gate, propose — merge only on operator approve.
const MergerSoulTemplate = "pipeline-merger"

// reviewerRole / fixerRole are the agents.role values for dispatched
// pipeline agents.
const (
	reviewerRole = "reviewer"
	fixerRole    = "fixer"
	mergerRole   = "merger"
)

// DefaultMaxReviewRounds is the fixer-loop cap (ADR-0005 AC 7): the
// request_changes that lands when a task has already burned this many review
// rounds parks it in Needs Human instead of changes_requested.
const DefaultMaxReviewRounds = 3

// queryRow is the minimal surface shared by *pgxpool.Pool and pgx.Tx for
// single-row reads inside or outside a tx.
type queryRow interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

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
	// ReviewTimeout is the stall watchdog bound: a live reviewer or fixer
	// with no outbox activity and no transcript growth for this long parks
	// the task in pending_approval (default 2h, MAQUINISTA_REVIEW_TIMEOUT).
	ReviewTimeout time.Duration
	// MaxReviewRounds is the fixer-loop cap (default 3,
	// MAQUINISTA_REVIEW_ROUNDS_MAX).
	MaxReviewRounds int
	// SessionName is the tmux session reviewers run in (window cleanup).
	SessionName string
	// ImplementorIdleAfter is the stuck-implementor self-heal bound (MAQ-14):
	// when a reviewer/fixer spawn fails on uq_agents_task_live and the
	// blocking live row is the task's implementor with no outbox activity
	// for this long, the row is auto-retired so the pipeline proceeds
	// (default 10m, MAQUINISTA_IMPLEMENTOR_IDLE_AFTER).
	ImplementorIdleAfter time.Duration
	// Gh drives the MAQ-16 PR surface: posting the round's verdict comment
	// and reading human PR comments into the next round's prompt. nil
	// disables both (flows skip silently — GitHub stays optional).
	Gh GhRunner
}

// DefaultImplementorIdleAfter is the MAQ-14 self-heal bound: an implementor
// whose pane streamed nothing for this long after the task reached 'review'
// has ended its turn without retiring.
const DefaultImplementorIdleAfter = 10 * time.Minute

// DefaultDispatchConfig returns the documented defaults.
func DefaultDispatchConfig(sessionName string) DispatchConfig {
	return DispatchConfig{
		Interval:             10 * time.Second,
		ReviewTimeout:        2 * time.Hour,
		MaxReviewRounds:      DefaultMaxReviewRounds,
		SessionName:          sessionName,
		ImplementorIdleAfter: DefaultImplementorIdleAfter,
	}
}

// DispatchConfigFromEnv applies MAQUINISTA_REVIEW_TIMEOUT,
// MAQUINISTA_REVIEW_ROUNDS_MAX and MAQUINISTA_IMPLEMENTOR_IDLE_AFTER over
// the defaults.
func DispatchConfigFromEnv(sessionName string) DispatchConfig {
	cfg := DefaultDispatchConfig(sessionName)
	if v := strings.TrimSpace(os.Getenv("MAQUINISTA_REVIEW_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.ReviewTimeout = d
		} else {
			log.Printf("pipeline: dispatch: invalid MAQUINISTA_REVIEW_TIMEOUT %q, using %s", v, cfg.ReviewTimeout)
		}
	}
	if v := strings.TrimSpace(os.Getenv("MAQUINISTA_IMPLEMENTOR_IDLE_AFTER")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.ImplementorIdleAfter = d
		} else {
			log.Printf("pipeline: dispatch: invalid MAQUINISTA_IMPLEMENTOR_IDLE_AFTER %q, using %s", v, cfg.ImplementorIdleAfter)
		}
	}
	if v := strings.TrimSpace(os.Getenv("MAQUINISTA_REVIEW_ROUNDS_MAX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxReviewRounds = n
		} else {
			log.Printf("pipeline: dispatch: invalid MAQUINISTA_REVIEW_ROUNDS_MAX %q, using %d", v, cfg.MaxReviewRounds)
		}
	}
	return cfg
}

// ReviewSpawnParams carries what the spawner needs to materialize one
// dispatched pane (reviewer or fixer). AgentID is pre-minted by dispatch;
// Role/SoulTemplateID are set by the dispatch passes (reviewer defaults kept
// for EX-03-era callers that leave them empty).
type ReviewSpawnParams struct {
	AgentID        string
	TaskID         string
	WorktreePath   string
	Role           string // "reviewer" (default) or "fixer"
	SoulTemplateID string // "" = pipeline-reviewer
	RunnerType     string // extras default_runner override ("" = cfg default)
	Model          string // resolved from reasoning_class ("" = runner default)
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
	if cfg.MaxReviewRounds <= 0 {
		cfg.MaxReviewRounds = DefaultMaxReviewRounds
	}
	if cfg.ImplementorIdleAfter <= 0 {
		cfg.ImplementorIdleAfter = DefaultImplementorIdleAfter
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := dispatchPass(ctx, pool, cfg.Gh, spawn, cfg.ImplementorIdleAfter); err != nil {
			log.Printf("pipeline: dispatch: spawn pass: %v", err)
		}
		if err := promptPass(ctx, pool, cfg.Gh); err != nil {
			log.Printf("pipeline: dispatch: prompt pass: %v", err)
		}
		if err := verdictPass(ctx, pool, cfg.Gh, cfg.MaxReviewRounds, cfg.SessionName, killWindow); err != nil {
			log.Printf("pipeline: dispatch: verdict pass: %v", err)
		}
		if err := fixerPass(ctx, pool, spawn, cfg.ImplementorIdleAfter); err != nil {
			log.Printf("pipeline: dispatch: fixer pass: %v", err)
		}
		if err := watchdogPass(ctx, pool, cfg.ReviewTimeout, cfg.SessionName, killWindow); err != nil {
			log.Printf("pipeline: dispatch: watchdog pass: %v", err)
		}
		if err := mergeEnqueuePass(ctx, pool); err != nil {
			log.Printf("pipeline: dispatch: merge enqueue pass: %v", err)
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

func dispatchPass(ctx context.Context, pool *pgxpool.Pool, g GhRunner, spawn ReviewSpawner, idleAfter time.Duration) error {
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
			AgentID:        agentID,
			TaskID:         c.taskID,
			WorktreePath:   c.worktree,
			Role:           reviewerRole,
			SoulTemplateID: ReviewerSoulTemplate,
			RunnerType:     runnerType,
			Model:          model,
		})
		if err != nil {
			if handleUniqueLiveBlocker(ctx, pool, c.taskID, err, idleAfter, "reviewer") {
				continue
			}
			// Includes the cross-process unique-live loss; next tick no-ops.
			log.Printf("pipeline: dispatch: spawn reviewer %s for %s: %v", agentID, c.taskID, err)
			continue
		}
		if err := recordReviewRound(ctx, pool, g, agentID, c.taskID); err != nil {
			log.Printf("pipeline: dispatch: record round %s: %v", c.taskID, err)
		}
		log.Printf("pipeline: dispatch: spawned reviewer %s for task %s (worktree %s)", agentID, c.taskID, c.worktree)
	}
	return nil
}

// ---- stuck-implementor self-heal (MAQ-14) -------------------------------
//
// A task flips to 'review' when its implementor runs set-pr — which records
// the PR but touches no agents row. Only a maquinista-done (or the
// `maquinista tasks release` cleanup) retires the implementor, and an
// implementor that ends its turn right after opening the PR leaves the row
// 'running' forever. The uq_agents_task_live index then blocks every
// reviewer/fixer spawn for the task until a human retires the row by hand.
// The self-heal: on a unique-live spawn failure, if the blocking live row IS
// the task's implementor and its last outbox activity is older than
// idleAfter, the turn is over for good — retire it (the guarded UPDATE fires
// at most once, which is also the exactly-once notification dedup) and let
// the next tick spawn the reviewer. Fresh, still-streaming implementors are
// left alone; non-implementor blockers (reviewer/fixer rows) keep the
// generic retry path.

// isUniqueLiveErr reports the one-live-agent-per-task violation
// (SQLSTATE 23505 on uq_agents_task_live) surfaced by the agents INSERT in
// SpawnFresh — the same shape EnsureAgent guards for the scheduler path.
func isUniqueLiveErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "uq_agents_task_live")
}

// implementorLiveSQL: the live implementor row bound to a task (uq index
// guarantees at most one), with its outbox idleness in seconds — last
// agent_outbox write, falling back to started_at for agents that never
// streamed.
const implementorLiveSQL = `
SELECT a.id,
       EXTRACT(EPOCH FROM NOW() - COALESCE(
             (SELECT MAX(o.created_at) FROM agent_outbox o WHERE o.agent_id = a.id),
             a.started_at))
FROM agents a
WHERE a.task_id = $1 AND a.status <> 'dead' AND a.role = 'implementor'`

// retireStuckImplementor retires the task's implementor row when it has been
// outbox-idle past idleAfter (see the package comment above). Returns
// retired=true when THIS call performed the retire (notification included);
// live=true when a live implementor row exists but is not stale enough yet.
func retireStuckImplementor(ctx context.Context, pool *pgxpool.Pool, taskID string, idleAfter time.Duration) (retired, live bool, err error) {
	var agentID string
	var idleSecs float64
	err = pool.QueryRow(ctx, implementorLiveSQL, taskID).Scan(&agentID, &idleSecs)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil // blocker is a reviewer/fixer row — not ours
	}
	if err != nil {
		return false, false, err
	}
	if time.Duration(idleSecs*float64(time.Second)) < idleAfter {
		return false, true, nil // still streaming — keep retrying silently
	}
	tag, err := pool.Exec(ctx, `
		UPDATE agents SET status='dead', last_seen=NOW()
		WHERE id=$1 AND status <> 'dead' AND role='implementor'
	`, agentID)
	if err != nil {
		return false, true, err
	}
	if tag.RowsAffected() == 0 {
		return false, true, nil // raced to dead elsewhere — no notification
	}
	notifyf(ctx, pool, "🆘 %s: implementor %s ended its turn without retiring (idle > %s, no completion processed) — auto-retired it; review proceeds. If the PR looks complete this needs no action.%s",
		taskTitle(ctx, pool, taskID), agentID, idleAfter, prLinkSuffix(ctx, pool, taskID))
	return true, true, nil
}

// handleUniqueLiveBlocker classifies a spawn failure. When it is the
// unique-live violation it runs the stuck-implementor self-heal and reports
// whether the caller should stay silent (retired, or implementor still
// fresh); false means "not our case, log the error generically".
func handleUniqueLiveBlocker(ctx context.Context, pool *pgxpool.Pool, taskID string, err error, idleAfter time.Duration, arm string) bool {
	if !isUniqueLiveErr(err) {
		return false
	}
	retired, _, herr := retireStuckImplementor(ctx, pool, taskID, idleAfter)
	if herr != nil {
		log.Printf("pipeline: dispatch: stuck-implementor check %s: %v", taskID, herr)
		return true
	}
	if retired {
		log.Printf("pipeline: dispatch: auto-retired stuck implementor on %s (%s spawn was uq_agents_task_live-blocked) — spawns next tick", taskID, arm)
	}
	// Fresh implementor or handled retire: silent retry until the idle bound.
	return true
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

// mintAgentID picks the first <role>-<taskID>[-rN] id not present in agents
// (same shape as orchestrator.mintAgentID, kept local to avoid the
// cross-package dependency).
func mintAgentID(ctx context.Context, pool *pgxpool.Pool, role, taskID string) (string, error) {
	base := role + "-" + taskID
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
	return "", fmt.Errorf("mintAgentID: exhausted retries for %s", base)
}

func mintReviewerID(ctx context.Context, pool *pgxpool.Pool, taskID string) (string, error) {
	return mintAgentID(ctx, pool, reviewerRole, taskID)
}

// resolveTemplateExec reads the reviewer template's frozen extras contract
// and resolves the (runner, model) pair. jsonb scans as []byte (pgx v5
// returns map[string]any for `any` targets — the role-souls gotcha).
// runnerOverride/model may be empty: empty runner = cfg default, empty model
// = the runner's own resolution chain (MAQUINISTA_PI_MODEL env or default).
func resolveTemplateExec(ctx context.Context, pool *pgxpool.Pool) (runnerOverride, model string, err error) {
	return resolveTemplateExecFor(ctx, pool, ReviewerSoulTemplate)
}

// resolveTemplateExecFor is the template-parameterized resolver: reads the
// named soul template's frozen extras (default_runner, reasoning_class) and
// maps them through ResolveExec. Dispatch reads templates, never writes.
func resolveTemplateExecFor(ctx context.Context, pool *pgxpool.Pool, templateID string) (runnerOverride, model string, err error) {
	var raw []byte
	err = pool.QueryRow(ctx, `SELECT extras FROM soul_templates WHERE id = $1`, templateID).Scan(&raw)
	if err != nil {
		return "", "", fmt.Errorf("load template %s: %w", templateID, err)
	}
	var ex struct {
		DefaultRunner  string `json:"default_runner"`
		ReasoningClass string `json:"reasoning_class"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		return "", "", fmt.Errorf("parse extras of %s: %w", templateID, err)
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

// recordReviewRound increments review_rounds and enqueues the review prompt.
// Two commits, deliberately: the bump commits FIRST (its presence makes the
// next tick a no-op for the spawn pass), then the prompt is built (the MAQ-16
// human-comment fetch runs with NO transaction open — GitHub latency must
// never hold a DB tx) and enqueued in its own tx. A crash between the two
// heals via promptPass: the round's prompt row is missing and the heal's
// EnqueueInbox dedup (origin_channel, external_msg_id) re-enqueues exactly
// once.
func recordReviewRound(ctx context.Context, pool *pgxpool.Pool, g GhRunner, agentID, taskID string) error {
	var round int
	if err := pool.QueryRow(ctx, `
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
		"prompt":  buildReviewPrompt(ctx, pool, g, taskID, round, agentID),
	})
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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
// humanComments (rendered by renderHumanComments) is appended when non-empty:
// the PR's human comments newer than the previous reviewer, framed as verdict
// INPUT (MAQ-16 — they are never approve/request_changes verbs).
func reviewPromptBody(taskID string, round int, humanComments string) string {
	body := fmt.Sprintf(
		"Review round %d for task %s. The implementation is committed in your cwd (the task worktree). "+
			"Run `git fetch origin && git diff origin/main...HEAD` (plus `git log origin/main..HEAD`) to see the change, "+
			"read the spec under .specs/ if present, run the validators the spec names, and judge the change on its merits. "+
			"End your reply with exactly one line: VERDICT: approve | VERDICT: request_changes | VERDICT: needs_human.",
		round, taskID)
	if humanComments != "" {
		body += "\n\n" + humanComments
	}
	return body
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

func promptPass(ctx context.Context, pool *pgxpool.Pool, g GhRunner) error {
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
	for _, gap := range gaps {
		log.Printf("pipeline: dispatch: healing missing review prompt %s round %d", gap.taskID, gap.round)
		if err := enqueueReviewPrompt(ctx, pool, g, gap.agentID, gap.taskID, gap.round); err != nil {
			log.Printf("pipeline: dispatch: heal prompt %s: %v", gap.taskID, err)
		}
	}
	return nil
}

func enqueueReviewPrompt(ctx context.Context, pool *pgxpool.Pool, g GhRunner, agentID, taskID string, round int) error {
	// Build with no transaction open (see recordReviewRound).
	content, err := json.Marshal(map[string]any{
		"type": "review", "task_id": taskID, "round": round,
		"prompt": buildReviewPrompt(ctx, pool, g, taskID, round, agentID),
	})
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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
SELECT a.id, a.tmux_session, a.tmux_window, t.id, t.title, t.review_rounds
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE a.role = '` + reviewerRole + `'
  AND a.status <> 'dead'
  AND t.status = 'review'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL`

func verdictPass(ctx context.Context, pool *pgxpool.Pool, g GhRunner, maxRounds int, sessionName string, killWindow func(session, windowID string) error) error {
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
		// MAQ-16: surface the verdict on the PR BEFORE the guarded
		// transition — a crash between the two heals on the next tick
		// (verdict re-parsed, round-marker dedup skips the repost), while
		// the other order would lose the comment (applyVerdict retires the
		// reviewer). Best-effort: a gh outage logs and never blocks the
		// transition.
		postReviewVerdictComment(ctx, pool, g, r.agentID, r.taskID, r.round, verdict)
		landed, applied, err := applyVerdict(ctx, pool, r.agentID, r.taskID, verdict, status, maxRounds)
		if err != nil {
			log.Printf("pipeline: dispatch: apply verdict %s → %s: %v", r.taskID, verdict, err)
			continue
		}
		if !applied {
			continue // task row raced to another status; leave the pane be
		}
		if landed != status {
			log.Printf("pipeline: dispatch: verdict %s on %s (reviewer %s) → %s (round cap %d reached — needs human)", verdict, r.taskID, r.agentID, landed, maxRounds)
		} else {
			log.Printf("pipeline: dispatch: verdict %s on %s (reviewer %s) → %s", verdict, r.taskID, r.agentID, status)
		}
		// EX-06: the verdict summary IS the merge proposal (approve) or
		// the needs-human question — emitted inside the applied
		// transition, so exactly once per verdict.
		notifyVerdict(ctx, pool, r.taskID, r.taskTitle, verdict, landed, r.round, maxRounds)
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
// reviewer — one tx, guarded on the task still being in 'review'. The round
// cap is decided ATOMICALLY inside the UPDATE: a request_changes landing at
// or past maxRounds burned rounds parks the task in pending_approval
// instead. Returns the landed status; applied=false when the row raced
// (nothing written).
func applyVerdict(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, verdict, status string, maxRounds int) (string, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	var landed string
	err = tx.QueryRow(ctx, `
		UPDATE tasks SET status = CASE
			WHEN $2::text = 'changes_requested' AND review_rounds >= $3::int THEN 'pending_approval'
			ELSE $2::text END
		WHERE id = $1 AND status = 'review'
		RETURNING status
	`, taskID, status, maxRounds).Scan(&landed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	content := "VERDICT: " + verdict
	if landed != status {
		content = fmt.Sprintf("VERDICT: %s (round cap %d reached — needs human)", verdict, maxRounds)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'verdict', $3)
	`, taskID, agentID, content); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1`, agentID); err != nil {
		return "", false, err
	}
	return landed, true, tx.Commit(ctx)
}

// watchdogPass parks stalled pipeline agents: a live reviewer (in 'review')
// or fixer (in 'changes_requested') with NO outbox activity (the monitor
// writes rows as the agent streams) AND no transcript growth
// (agents.last_transcript_at, MAQ-9) for longer than the timeout flips the
// task to pending_approval and retires the pane. Agents younger than the
// timeout are exempt: a freshly spawned reviewer has no outbox rows yet
// (prompt delivery races pi's cold boot), and parking it on sight murders
// every slow-booting spawn before its first streamed token.
// liveReviewer is one live reviewer/fixer pane on a pipeline task. The
// title/round columns feed the EX-06 Pipeline-topic summaries.
type liveReviewer struct {
	agentID, session, window, taskID string
	taskTitle                        string
	round                            int
}

// liveFixersSQL drives the fixer watchdog arm: live fixer agents on
// pipeline tasks still in changes_requested.
const liveFixersSQL = `
SELECT a.id, a.tmux_session, a.tmux_window, t.id, t.title, t.review_rounds
FROM agents a
JOIN tasks t ON t.id = a.task_id
WHERE a.role = '` + fixerRole + `'
  AND a.status <> 'dead'
  AND t.status = 'changes_requested'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL`

func watchdogPass(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, sessionName string, killWindow func(session, windowID string) error) error {
	// Stall = silent on BOTH activity channels for the whole window:
	//   - no agent_outbox rows (assistant text streamed by the monitor), AND
	//   - no transcript growth (last_transcript_at — a healthy agent
	//     mid-command streams tool events into the JSONL but writes no
	//     outbox text; MAQ-9).
	// NULL last_transcript_at = no growth ever observed. The started_at age
	// guard keeps newborns (no signal on either channel yet) untouchable.
	stallFilter := `
  AND a.started_at < NOW() - make_interval(secs => $1)
  AND NOT EXISTS (
        SELECT 1 FROM agent_outbox o
        WHERE o.agent_id = a.id AND o.created_at > NOW() - make_interval(secs => $1))
  AND (a.last_transcript_at IS NULL
       OR a.last_transcript_at < NOW() - make_interval(secs => $1))`
	var reviewers []liveReviewer
	if err := scanReviewers(ctx, pool, liveReviewersSQL+stallFilter,
		[]any{timeout.Seconds()}, &reviewers); err != nil {
		return err
	}
	for _, r := range reviewers {
		applied, err := parkTask(ctx, pool, r.agentID, r.taskID, fmt.Sprintf("watchdog: review stalled past %s — needs human", timeout), "review")
		if err != nil {
			return err
		}
		if applied {
			log.Printf("pipeline: dispatch: watchdog retired stalled reviewer %s on %s → needs_human", r.agentID, r.taskID)
			notifyf(ctx, pool, "🆘 %s: %s%s", r.taskTitle,
				fmt.Sprintf("watchdog: review stalled past %s — needs human", timeout),
				prLinkSuffix(ctx, pool, r.taskID))
			killReviewerPane(sessionName, r.session, r.window, killWindow)
		}
	}

	var fixers []liveReviewer
	if err := scanReviewers(ctx, pool, liveFixersSQL+stallFilter,
		[]any{timeout.Seconds()}, &fixers); err != nil {
		return err
	}
	for _, r := range fixers {
		applied, err := parkTask(ctx, pool, r.agentID, r.taskID, fmt.Sprintf("watchdog: fix stalled past %s — needs human", timeout), "changes_requested")
		if err != nil {
			return err
		}
		if applied {
			log.Printf("pipeline: dispatch: watchdog retired stalled fixer %s on %s → needs_human", r.agentID, r.taskID)
			notifyf(ctx, pool, "🆘 %s: %s%s", r.taskTitle,
				fmt.Sprintf("watchdog: fix stalled past %s — needs human", timeout),
				prLinkSuffix(ctx, pool, r.taskID))
			killReviewerPane(sessionName, r.session, r.window, killWindow)
		}
	}
	return nil
}

// parkTask flips the task to pending_approval (guarded on its current
// status), records the watchdog note as a verdict row, and retires the
// agent — one tx. applied=false when the row raced to another status.
func parkTask(ctx context.Context, pool *pgxpool.Pool, agentID, taskID, note, expectStatus string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE tasks SET status='pending_approval' WHERE id=$1 AND status=$2`, taskID, expectStatus)
	if err != nil {
		tx.Rollback(ctx)
		return false, err
	}
	if tag.RowsAffected() == 0 {
		tx.Rollback(ctx)
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'verdict', $3)
	`, taskID, agentID, note); err != nil {
		tx.Rollback(ctx)
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agents SET status='dead', last_seen=NOW() WHERE id=$1`, agentID); err != nil {
		tx.Rollback(ctx)
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ---- fixer loop (EX-04) -------------------------------------------------
//
// A task parked in changes_requested by a request_changes verdict is
// re-claimed by a fresh fixer session working the SAME worktree/PR. The
// episode is keyed by review_rounds (frozen while parked — it only bumps at
// the next reviewer spawn); the fix row + inbox dedup + unique-live index
// are the three idempotency guards. The fixer completes via maquinista-done
// → db.MarkDone → 'review' (done-path branch), where the next reviewer spawn
// bumps the round: the loop closes with no new transition code. No
// zero-author guard here BY DESIGN — a fixer continuing the previous fixer's
// work is the point; the reviewer at round N+1 is always a fresh mint.

// fixerCandidatesSQL: changes_requested pipeline tasks with a worktree, a
// request_changes verdict, no live fixer, and no fix row for the current
// episode. Role-scoped live check (symmetric with reviewCandidatesSQL):
// cross-role windows (a reviewer pane the verdict pass failed to kill) rely
// on uq_agents_task_live + the next-tick retry, same as EX-03.
const fixerCandidatesSQL = `
SELECT t.id, t.worktree_path, v.agent_id
FROM tasks t
JOIN LATERAL (
    SELECT agent_id FROM task_context
    WHERE task_id = t.id AND kind = 'verdict'
      AND content LIKE 'VERDICT: request_changes%'
    ORDER BY created_at DESC LIMIT 1
) v ON TRUE
WHERE t.status = 'changes_requested'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL
  AND t.worktree_path IS NOT NULL AND t.worktree_path <> ''
  AND NOT EXISTS (
        SELECT 1 FROM agents a
        WHERE a.task_id = t.id AND a.status <> 'dead' AND a.role = '` + fixerRole + `')
  AND NOT EXISTS (
        SELECT 1 FROM task_context f
        WHERE f.task_id = t.id AND f.kind = 'fix'
          AND f.content = 'round ' || t.review_rounds::text)`

// fixerPromptHealSQL: a live fixer whose episode's prompt row is missing
// (crash between spawn and enqueue) — healed exactly once by the dedup'd
// external_msg_id.
const fixerPromptHealSQL = `
SELECT a.id, t.id, t.review_rounds, v.agent_id
FROM tasks t
JOIN agents a ON a.task_id = t.id AND a.status <> 'dead' AND a.role = '` + fixerRole + `'
JOIN LATERAL (
    SELECT agent_id FROM task_context
    WHERE task_id = t.id AND kind = 'verdict'
      AND content LIKE 'VERDICT: request_changes%'
    ORDER BY created_at DESC LIMIT 1
) v ON TRUE
WHERE t.status = 'changes_requested'
  AND t.metadata->>'ticket_issue_id' IS NOT NULL
  AND NOT EXISTS (
        SELECT 1 FROM agent_inbox i
        WHERE i.agent_id = a.id
          AND i.origin_channel = 'task'
          AND i.external_msg_id = 'fix:' || t.id || ':' || t.review_rounds)`

// maxFindingsChars bounds the reviewer-message excerpt embedded in the fix
// prompt (the soul mandates a numbered findings list at the top of the final
// reply; the tail keeps the prompt bounded).
const maxFindingsChars = 6000

func fixerPass(ctx context.Context, pool *pgxpool.Pool, spawn ReviewSpawner, idleAfter time.Duration) error {
	rows, err := pool.Query(ctx, fixerCandidatesSQL)
	if err != nil {
		return err
	}
	type cand struct{ taskID, worktree, reviewerAgent string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.taskID, &c.worktree, &c.reviewerAgent); err != nil {
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
		agentID, err := mintAgentID(ctx, pool, fixerRole, c.taskID)
		if err != nil {
			log.Printf("pipeline: dispatch: mint fixer %s: %v", c.taskID, err)
			continue
		}
		runnerType, model, err := resolveTemplateExecFor(ctx, pool, FixerSoulTemplate)
		if err != nil {
			log.Printf("pipeline: dispatch: resolve fixer exec for %s: %v", c.taskID, err)
		}
		err = spawn.SpawnReviewer(ctx, ReviewSpawnParams{
			AgentID:        agentID,
			TaskID:         c.taskID,
			WorktreePath:   c.worktree,
			Role:           fixerRole,
			SoulTemplateID: FixerSoulTemplate,
			RunnerType:     runnerType,
			Model:          model,
		})
		if err != nil {
			if handleUniqueLiveBlocker(ctx, pool, c.taskID, err, idleAfter, "fixer") {
				continue
			}
			// Includes the cross-process unique-live loss; next tick no-ops.
			log.Printf("pipeline: dispatch: spawn fixer %s for %s: %v", agentID, c.taskID, err)
			continue
		}
		// Reviewer round now parked in changes_requested = the episode being
		// fixed; round is frozen until the next reviewer spawn bumps it.
		var round int
		if err := pool.QueryRow(ctx,
			`SELECT review_rounds FROM tasks WHERE id = $1`, c.taskID).Scan(&round); err != nil {
			log.Printf("pipeline: dispatch: fixer round lookup %s: %v", c.taskID, err)
			continue
		}
		if err := recordFixEpisode(ctx, pool, agentID, c.taskID, round, c.reviewerAgent); err != nil {
			// Prompt miss heals on the next tick (fixerPromptHealSQL).
			log.Printf("pipeline: dispatch: record fix episode %s: %v", c.taskID, err)
		}
		log.Printf("pipeline: dispatch: spawned fixer %s for task %s (round %d, worktree %s)", agentID, c.taskID, round, c.worktree)
	}
	return fixerPromptPass(ctx, pool)
}

// fixerPromptPass heals the crash-between-spawn-and-enqueue case for fixers
// (mirrors promptPass for reviewers).
func fixerPromptPass(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, fixerPromptHealSQL)
	if err != nil {
		return err
	}
	type gap struct{ agentID, taskID string; round int; reviewerAgent string }
	var gaps []gap
	for rows.Next() {
		var g gap
		if err := rows.Scan(&g.agentID, &g.taskID, &g.round, &g.reviewerAgent); err != nil {
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
		log.Printf("pipeline: dispatch: healing missing fix prompt %s round %d", g.taskID, g.round)
		tx, err := pool.Begin(ctx)
		if err != nil {
			log.Printf("pipeline: dispatch: heal fix prompt %s: %v", g.taskID, err)
			continue
		}
		if err := enqueueFixPrompt(ctx, tx, g.agentID, g.taskID, g.round, g.reviewerAgent); err != nil {
			tx.Rollback(ctx)
			log.Printf("pipeline: dispatch: heal fix prompt %s: %v", g.taskID, err)
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			log.Printf("pipeline: dispatch: heal fix prompt %s: %v", g.taskID, err)
		}
	}
	return nil
}

// latestFindings returns the reviewer's newest assistant message — the same
// row latestVerdict parses; per the soul contract the reply body IS the
// numbered findings list. Tail-capped to maxFindingsChars.
func latestFindings(ctx context.Context, q queryRow, reviewerAgent string) (string, error) {
	var text *string
	err := q.QueryRow(ctx, `
		SELECT content->>'text' FROM agent_outbox
		WHERE agent_id = $1 AND content ? 'text'
		ORDER BY created_at DESC LIMIT 1
	`, reviewerAgent).Scan(&text)
	if err != nil {
		return "", err
	}
	if text == nil {
		return "", fmt.Errorf("no findings text from %s", reviewerAgent)
	}
	t := *text
	if len(t) > maxFindingsChars {
		t = t[len(t)-maxFindingsChars:]
	}
	return t, nil
}

// recordFixEpisode inserts the episode marker (task_context kind 'fix',
// content 'round <N>' — the dedup key fixerCandidatesSQL reads) and then
// enqueues the fix prompt. The marker commits FIRST and ALONE: its presence,
// not the prompt's, is what stops re-spawning for this episode (a findings
// load failure must not un-bound the fixer mint — the prompt miss heals via
// fixerPromptPass, and a permanently missing findings text is bounded by the
// watchdog parking the task). The inbox insert dedups on
// (origin_channel, external_msg_id).
func recordFixEpisode(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, round int, reviewerAgent string) error {
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'fix', $3)
	`, taskID, agentID, fmt.Sprintf("round %d", round)); err != nil {
		return fmt.Errorf("insert fix row: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := enqueueFixPrompt(ctx, tx, agentID, taskID, round, reviewerAgent); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// fixerPromptBody is the per-episode briefing. The fixer soul carries the
// full method (findings top to bottom, proofs re-run, no scope growth); this
// carries the findings and the round specifics.
func fixerPromptBody(taskID string, round int, findings string) string {
	return fmt.Sprintf(
		"Fix round for task %s (review round %d failed). The reviewer's findings follow. "+
			"Your cwd is the task worktree — same branch, same PR as the rounds before. "+
			"Resolve every finding, re-run the affected proofs, and push. "+
			"Finish with: maquinista-done %s \"<summary naming the findings resolved>\".\n\n"+
			"Reviewer findings:\n%s",
		taskID, round, taskID, findings)
}

func enqueueFixPrompt(ctx context.Context, tx pgx.Tx, agentID, taskID string, round int, reviewerAgent string) error {
	findings, err := latestFindings(ctx, tx, reviewerAgent)
	if err != nil {
		return fmt.Errorf("load findings: %w", err)
	}
	content, err := json.Marshal(map[string]any{
		"type":    "fix",
		"task_id": taskID,
		"round":   round,
		"prompt":  fixerPromptBody(taskID, round, findings),
	})
	if err != nil {
		return err
	}
	_, _, err = mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: fmt.Sprintf("fix:%s:%d", taskID, round),
		Content:       content,
	})
	return err
}

func scanReviewers(ctx context.Context, pool *pgxpool.Pool, sql string, args []any, out *[]liveReviewer) error {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r liveReviewer
		if err := rows.Scan(&r.agentID, &r.session, &r.window, &r.taskID, &r.taskTitle, &r.round); err != nil {
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
