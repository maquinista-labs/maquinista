// GitHub comment commands (MAQ-12): the PR conversation as a control
// surface. An allowed GitHub login comments `maquinista <verb> [args...]`
// on a PR; the poller parses the command, resolves the target task from the
// PR alone (id-less — the PR maps 1:1 to the task via tasks.pr_url), and
// routes it through a verb dispatch table. `approve` ships first; adding a
// verb is nothing more than a RegisterCommentVerb call — parser, auth,
// resolution, exactly-once claiming and acks are all shared.
//
// Mechanics (MAQ-12 frame):
//   - polling only, via the gh CLI behind CommentSource — no webhooks (the
//     box is home infra, no public endpoint). One comments fetch per
//     watched PR per tick; cadence default 60s, floor 30s; collaborator
//     checks cached. Rate-limit safe by construction.
//   - exactly-once: the gh_comment_commands INSERT (PK = the globally
//     unique GitHub comment id) IS the claim — a duplicate command never
//     re-runs its verb, so it can never double-merge (the merge_queue
//     live-entry index is the second guard).
//   - auth: PIPELINE_GH_ALLOWED_LOGINS, falling back to a repo-collaborator
//     check via gh. Everyone else is ignored silently.
//   - no-op discipline: task not found or the verb not applicable to its
//     state → one clean no-op, no state damage. Transient GitHub/DB errors
//     are retried on the next pass (nothing is claimed until processing can
//     proceed).
//   - non-command comments are not dead traffic either (MAQ-30): an allowed
//     login's plain comment on a watched PR re-opens a round on the task
//     behind it (dispatchCommentRound → triggerCommentRound — the state
//     machine lives in reround.go). Bots and non-allowed logins never
//     trigger; both are claimed, so the silence is remembered.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// PRComment is one PR conversation comment (a GitHub issue comment on the
// pull request). Provider-neutral shape; gh.Runner fills it in.
type PRComment struct {
	ID        int64
	Author    string // GitHub login
	IsBot     bool   // author is a bot/app account
	Body      string
	CreatedAt time.Time
}

// CommentSource is the GitHub side of the comment-command surface (the gh
// CLI in production, a fake in tests). Separate from GhRunner so the merge
// interface stays minimal and the two surfaces evolve independently.
type CommentSource interface {
	// PRComments returns the PR's conversation comments created after
	// since, oldest first.
	PRComments(ctx context.Context, pr int, since time.Time) ([]PRComment, error)
	// IsCollaborator reports whether login may push to the repo — the
	// default allowlist when PIPELINE_GH_ALLOWED_LOGINS is unset.
	IsCollaborator(ctx context.Context, login string) (bool, error)
	// ReactToComment adds a +1 reaction to the comment (the ack).
	ReactToComment(ctx context.Context, commentID int64) error
	// PRHeadBranch returns the PR's head branch name (resolution fallback).
	PRHeadBranch(ctx context.Context, pr int) (string, error)
}

// ---- config ----

const (
	// DefaultGhCommentsPoll is the poll cadence (MAQ-12: 30–60 s).
	DefaultGhCommentsPoll = 60 * time.Second
	// MinGhCommentsPoll is the enforced floor — a misconfigured sub-30 s
	// cadence would burn the GitHub rate budget for nothing.
	MinGhCommentsPoll = 30 * time.Second
	// DefaultGhCommentsCatchup is the window the first pass after daemon
	// start re-reads, so a command posted during a short outage is still
	// seen. Claims make the overlap exactly-once.
	DefaultGhCommentsCatchup = 10 * time.Minute
	// DefaultGhAuthCacheTTL bounds how long collaborator verdicts are
	// cached (membership changes take this long to notice).
	DefaultGhAuthCacheTTL = 10 * time.Minute
)

// GhCommandsConfig carries the comment-command knobs.
type GhCommandsConfig struct {
	// AllowedLogins is the explicit allowlist (PIPELINE_GH_ALLOWED_LOGINS,
	// comma/space separated). Empty → repo-collaborator check via gh.
	AllowedLogins []string
	// Interval between passes (default 60s, floor 30s,
	// PIPELINE_GH_COMMENTS_POLL).
	Interval time.Duration
	// Catchup is the initial read window after start (default 10m).
	Catchup time.Duration
}

// GhCommandsConfigFromEnv reads PIPELINE_GH_ALLOWED_LOGINS and
// PIPELINE_GH_COMMENTS_POLL. The CommentSource is wired by the caller —
// env only selects behavior, never binaries.
func GhCommandsConfigFromEnv() GhCommandsConfig {
	cfg := GhCommandsConfig{
		Interval: DefaultGhCommentsPoll,
		Catchup:  DefaultGhCommentsCatchup,
	}
	if v := os.Getenv("PIPELINE_GH_ALLOWED_LOGINS"); v != "" {
		for _, f := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == ';' || r == '\t'
		}) {
			if f = strings.TrimSpace(f); f != "" {
				cfg.AllowedLogins = append(cfg.AllowedLogins, f)
			}
		}
	}
	if v := os.Getenv("PIPELINE_GH_COMMENTS_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Interval = d
		} else {
			log.Printf("pipeline: invalid PIPELINE_GH_COMMENTS_POLL %q — using %s", v, cfg.Interval)
		}
	}
	if cfg.Interval < MinGhCommentsPoll {
		cfg.Interval = MinGhCommentsPoll
	}
	return cfg
}

// ---- parser ----

// commentCmdRe matches the FIRST non-empty line of a command comment:
// optional leading `/`, the literal "maquinista" (case-insensitive), one or
// more spaces, the verb, optional args. Anything else before the word makes
// it prose, not a command.
var commentCmdRe = regexp.MustCompile(`(?i)^[ \t]*/?[ \t]*maquinista[ \t]+(\S+)(?:[ \t]+(.+))?$`)

// ParseCommentCommand parses `maquinista <verb> [args...]` out of a comment
// body — case-insensitive, tolerant of leading `/` and whitespace, first
// non-empty line only. ok=false for every non-command comment (which the
// dispatcher then ignores entirely).
func ParseCommentCommand(body string) (verb string, args []string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		m := commentCmdRe.FindStringSubmatch(trimmed)
		if m == nil {
			return "", nil, false
		}
		if m[2] != "" {
			args = strings.Fields(m[2])
		}
		return strings.ToLower(m[1]), args, true
	}
	return "", nil, false
}

// ---- dispatch table ----

// ErrNotApplicable: the verb does not apply to the resolved task's state.
// The dispatcher records a clean single no-op — never an error, never state
// damage (MAQ-12 AC 4).
var ErrNotApplicable = errors.New("verb not applicable to task state")

// CommentContext is everything a verb handler receives.
type CommentContext struct {
	Pool      *pgxpool.Pool
	Source    CommentSource // acks / fallback lookups (may be nil in tests)
	PR        int
	CommentID int64
	TaskID    string
	Actor     string // GitHub login
	Args      []string
	// Spawn materializes agent sessions for verbs that spawn one (resolve).
	// Nil in tests that don't exercise spawning; a nil here fails the verb
	// with a plain error (wiring bug, not a task-state no-op).
	Spawn ReviewSpawner
	// Merge plumbing for verbs that drive the PR merge flow.
	Merge  MergeConfig
	Prov   TicketProvider
	TeamID string
}

// CommentVerbHandler processes one parsed command against its resolved PR
// context. Return ErrNotApplicable for a clean no-op; any other error is
// recorded as 'error' (the merge machinery transitions guarded, so a retry
// is always safe).
type CommentVerbHandler func(ctx context.Context, hc CommentContext) error

var (
	commentVerbsMu sync.Mutex
	commentVerbs   = map[string]CommentVerbHandler{}
)

// RegisterCommentVerb registers a handler for a verb (lowercased on
// registration). This call is the entire extension surface — parser, auth,
// target resolution, idempotency and acks are shared (MAQ-12 AC 5).
func RegisterCommentVerb(verb string, h CommentVerbHandler) {
	commentVerbsMu.Lock()
	defer commentVerbsMu.Unlock()
	commentVerbs[strings.ToLower(verb)] = h
}

func unregisterCommentVerb(verb string) {
	commentVerbsMu.Lock()
	defer commentVerbsMu.Unlock()
	delete(commentVerbs, strings.ToLower(verb))
}

func commentVerbHandler(verb string) CommentVerbHandler {
	commentVerbsMu.Lock()
	defer commentVerbsMu.Unlock()
	return commentVerbs[strings.ToLower(verb)]
}

func init() {
	RegisterCommentVerb("approve", approveCommentHandler)
	RegisterCommentVerb("resolve", resolveCommentHandler)
}

// approveCommentHandler is the `approve` verb: merge the resolved task's PR
// now — the same arm the id-carrying `maquinista approve <task-id>` CLI
// takes in gh mode, but id-less: the PR context IS the target.
func approveCommentHandler(ctx context.Context, hc CommentContext) error {
	var status string
	if err := hc.Pool.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id = $1`, hc.TaskID).Scan(&status); err != nil {
		return fmt.Errorf("pipeline: approve comment: loading task %s: %w", hc.TaskID, err)
	}
	if status != "ready_to_merge" {
		return fmt.Errorf("%w: task %s is %s, want ready_to_merge", ErrNotApplicable, hc.TaskID, status)
	}
	if hc.Merge.Mode != MergeModeGH {
		return fmt.Errorf("%w: merge mode is %s, not gh", ErrNotApplicable, hc.Merge.Mode)
	}
	if err := RunMergeOnApprove(ctx, hc.Pool, hc.Merge, hc.Prov, hc.TeamID, hc.TaskID); err != nil {
		return err
	}
	// The merge flow posts its own result note; this one attributes the
	// approval to the GitHub commenter. RunMergeOnApprove has already run
	// the merge synchronously by the time this posts.
	notifyTaskf(ctx, hc.Pool, hc.TaskID, "👍 %s: approved via PR #%d comment by @%s — merge completed.",
		taskTitle(ctx, hc.Pool, hc.TaskID), hc.PR, hc.Actor)
	hc.ack(ctx)
	return nil
}

// ack reacts +1 to the command comment — a cheap visible "accepted"
// (MAQ-12 point 7). Best-effort; never fails the command.
func (hc CommentContext) ack(ctx context.Context) {
	if hc.Source == nil {
		return
	}
	if err := hc.Source.ReactToComment(ctx, hc.CommentID); err != nil {
		log.Printf("pipeline: ack reaction on comment %d: %v", hc.CommentID, err)
	}
}

// ---- resolve verb ----
//
// `maquinista resolve` on a parked PR spawns a merger session (the
// pipeline-merger soul, migration 035): rebase the branch onto origin/main,
// resolve the conflict files and the PR's pending review comments, push,
// and post the merge proposal. It does NOT merge — the operator's
// `maquinista approve` re-runs the merge gate on the now-clean branch.
// This is the human-triggered arm of the conflict-park path (EX-05 parks
// pending_approval with the conflict file list; this verb turns that park
// into work instead of a dead end).

// resolveCommentHandler is the `resolve` verb: spawn the merger session for
// the resolved task. Targets pending_approval — the state the merge flow
// parks on (rebase conflict, CI-attempt cap, watchdog park). approve is the
// verb for ready_to_merge; resolve on any other state is a clean no-op.
func resolveCommentHandler(ctx context.Context, hc CommentContext) error {
	if hc.Spawn == nil {
		return fmt.Errorf("pipeline: resolve comment: no spawner wired (cmd_start CommentDeps.Spawn)")
	}
	var status, worktree string
	if err := hc.Pool.QueryRow(ctx,
		`SELECT status, COALESCE(worktree_path, '') FROM tasks WHERE id = $1`,
		hc.TaskID).Scan(&status, &worktree); err != nil {
		return fmt.Errorf("pipeline: resolve comment: loading task %s: %w", hc.TaskID, err)
	}
	if status != "pending_approval" {
		return fmt.Errorf("%w: task %s is %s, want pending_approval (approve merges a ready_to_merge task)",
			ErrNotApplicable, hc.TaskID, status)
	}
	if worktree == "" {
		return fmt.Errorf("%w: task %s has no worktree — nothing to rebase", ErrNotApplicable, hc.TaskID)
	}
	branch, files := latestMergeEntry(ctx, hc.Pool, hc.TaskID)

	agentID, err := mintAgentID(ctx, hc.Pool, mergerRole, hc.TaskID)
	if err != nil {
		return fmt.Errorf("pipeline: resolve comment: mint merger %s: %w", hc.TaskID, err)
	}
	runnerType, model, err := resolveTemplateExecFor(ctx, hc.Pool, MergerSoulTemplate)
	if err != nil {
		// Non-fatal: empty overrides fall through to the runner's own
		// resolution chain (same stance as the fixer spawn).
		log.Printf("pipeline: resolve comment: resolve merger exec for %s: %v", hc.TaskID, err)
	}
	if err := hc.Spawn.SpawnReviewer(ctx, ReviewSpawnParams{
		AgentID:        agentID,
		TaskID:         hc.TaskID,
		WorktreePath:   worktree,
		Role:           mergerRole,
		SoulTemplateID: MergerSoulTemplate,
		RunnerType:     runnerType,
		Model:          model,
	}); err != nil {
		return fmt.Errorf("pipeline: resolve comment: spawn merger %s for %s: %w", agentID, hc.TaskID, err)
	}
	// Marker + prompt in one tx AFTER the spawn (fixer ordering): the
	// external_msg_id dedups a retry; the 'merge' row is the audit trail.
	if err := recordResolveEpisode(ctx, hc.Pool, agentID, hc.TaskID, hc.CommentID, branch, files); err != nil {
		return fmt.Errorf("pipeline: resolve comment: record episode %s (merger %s spawned, prompt may be missing): %w",
			hc.TaskID, agentID, err)
	}
	notifyTaskf(ctx, hc.Pool, hc.TaskID, "🔧 %s: resolve session %s spawned (PR #%d, branch %s) by @%s — rebase + pending comments; approve re-merges after.",
		taskTitle(ctx, hc.Pool, hc.TaskID), agentID, hc.PR, branch, hc.Actor)
	hc.ack(ctx)
	return nil
}

// latestMergeEntry returns the task's most recent merge_queue entry's branch
// and conflict file list — the resolve prompt names them so the session
// starts from the park's evidence, not a fresh probe. Best-effort: a lookup
// failure yields empty strings (the prompt then carries no file list and the
// session discovers the branch from its worktree).
func latestMergeEntry(ctx context.Context, pool *pgxpool.Pool, taskID string) (string, []string) {
	var branch string
	var files []string
	if err := pool.QueryRow(ctx,
		`SELECT branch, COALESCE(conflict_files, '{}') FROM merge_queue
		 WHERE task_id = $1 ORDER BY id DESC LIMIT 1`, taskID).Scan(&branch, &files); err != nil {
		return "", nil
	}
	return branch, files
}

// recordResolveEpisode inserts the audit marker (task_context kind 'merge',
// content 'resolve <comment-id>') and enqueues the resolve prompt — one tx,
// the inbox dedup on (origin_channel, external_msg_id) making a re-resolve
// of the same comment a no-op even across poller restarts.
func recordResolveEpisode(ctx context.Context, pool *pgxpool.Pool, agentID, taskID string, commentID int64, branch string, files []string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_context (task_id, agent_id, kind, content)
		VALUES ($1, $2, 'merge', $3)
	`, taskID, agentID, fmt.Sprintf("resolve %d", commentID)); err != nil {
		return fmt.Errorf("insert merge row: %w", err)
	}
	content, err := json.Marshal(map[string]any{
		"type":    "merge",
		"task_id": taskID,
		"prompt":  resolvePromptBody(taskID, branch, files),
	})
	if err != nil {
		return err
	}
	if _, _, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:       agentID,
		FromKind:      "system",
		FromID:        "pipeline",
		OriginChannel: "task",
		ExternalMsgID: fmt.Sprintf("resolve:%s:%d", taskID, commentID),
		Content:       content,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resolvePromptBody is the resolve session's briefing. The merger soul
// carries the method (rebase first, conservative resolution, propose-not-
// merge); this carries the specifics: the parked branch and conflict files.
func resolvePromptBody(taskID, branch string, files []string) string {
	b := fmt.Sprintf(
		"Resolve session for task %s (branch %s): the merge flow parked it. "+
			"Rebase the branch onto origin/main, resolving every conflict — "+
			"keep the resolved diff minimal and faithful to the PR's intent. "+
			"Then read the PR's review conversation and address every unresolved "+
			"comment thread (reply or resolve each; no new scope). "+
			"Re-run the proofs the resolution touched, push with --force-with-lease, "+
			"and end by posting the merge proposal comment on the PR — do not merge: "+
			"the operator's maquinista approve re-runs the merge gate.",
		taskID, branch)
	if len(files) > 0 {
		b += "\n\nParked conflict files:\n" + strings.Join(files, "\n")
	}
	return b
}

// ---- dispatcher ----

// CommentDeps bundles what the comment-command dispatcher needs.
type CommentDeps struct {
	Pool   *pgxpool.Pool
	Source CommentSource
	Auth   GhCommandsConfig
	Merge  MergeConfig
	Prov   TicketProvider
	TeamID string
	// Spawn materializes sessions for spawning verbs (resolve) and for the
	// MAQ-30 comment-triggered fixer rounds. The wiring passes the same
	// spawner the dispatch loop uses.
	Spawn ReviewSpawner
	// Gh posts the MAQ-25 pickup marker for a comment-triggered fixer round.
	// Nil skips the post (same optional-GitHub stance as dispatch).
	Gh GhRunner
}

// errNoTaskForPR: no task row matches the PR (id-less resolution missed).
var errNoTaskForPR = errors.New("no task for PR")

// Dispositions recorded in gh_comment_commands.
const (
	DispPending      = "pending"
	DispOK           = "ok"
	DispNoOp         = "no_op"
	DispUnauthorized = "unauthorized"
	DispError        = "error"
)

// DispatchCommentCommand handles one PR comment: parse → authorize →
// resolve the target task (id-less) → claim exactly-once → route to the
// verb handler. Non-command comments take the MAQ-30 trigger arm instead
// (dispatchCommentRound — same auth/claim discipline, state machine instead
// of a verb). Returns the disposition recorded. A transient error ("", err)
// claims nothing so the next pass retries the comment.
func DispatchCommentCommand(ctx context.Context, d CommentDeps, authz *commentAuthorizer, pr int, c PRComment) (string, error) {
	verb, args, ok := ParseCommentCommand(c.Body)
	if !ok {
		// Non-command: the MAQ-30 trigger candidate (a plain human comment
		// re-opening a round). Still gated on auth + task resolution inside.
		return dispatchCommentRound(ctx, d, authz, pr, c)
	}

	allowed, err := authorizeCommenter(ctx, d, authz, c.Author)
	if err != nil {
		return "", err // transient: retry next pass, nothing claimed
	}
	if !allowed {
		// Claim so the silence is remembered — a non-allowed login never
		// re-triggers parsing or gh calls (MAQ-12: ignored silently).
		if _, err := claimCommentCommand(ctx, d.Pool, c, verb, ""); err != nil {
			return "", err
		}
		if err := setCommentDisposition(ctx, d.Pool, c.ID, DispUnauthorized, "login not allowed"); err != nil {
			log.Printf("pipeline: comment %d disposition: %v", c.ID, err)
		}
		return DispUnauthorized, nil
	}

	taskID, err := resolveTaskByPR(ctx, d.Pool, d.Source, pr)
	if err != nil {
		if !errors.Is(err, errNoTaskForPR) {
			return "", err // transient
		}
		if _, cerr := claimCommentCommand(ctx, d.Pool, c, verb, ""); cerr != nil {
			return "", cerr
		}
		if derr := setCommentDisposition(ctx, d.Pool, c.ID, DispNoOp,
			fmt.Sprintf("no task maps to PR #%d", pr)); derr != nil {
			log.Printf("pipeline: comment %d disposition: %v", c.ID, derr)
		}
		return DispNoOp, nil
	}

	handler := commentVerbHandler(verb)
	if handler == nil {
		if _, cerr := claimCommentCommand(ctx, d.Pool, c, verb, taskID); cerr != nil {
			return "", cerr
		}
		if derr := setCommentDisposition(ctx, d.Pool, c.ID, DispNoOp,
			fmt.Sprintf("unknown verb %q", verb)); derr != nil {
			log.Printf("pipeline: comment %d disposition: %v", c.ID, derr)
		}
		return DispNoOp, nil
	}

	claimed, err := claimCommentCommand(ctx, d.Pool, c, verb, taskID)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "duplicate", nil // already processed — exactly-once holds
	}

	hc := CommentContext{
		Pool: d.Pool, Source: d.Source, PR: pr, CommentID: c.ID,
		TaskID: taskID, Actor: c.Author, Args: args,
		Spawn: d.Spawn,
		Merge: d.Merge, Prov: d.Prov, TeamID: d.TeamID,
	}
	err = handler(ctx, hc)

	disp, detail := DispOK, ""
	switch {
	case err == nil:
	case errors.Is(err, ErrNotApplicable):
		disp, detail = DispNoOp, err.Error()
	default:
		disp, detail = DispError, err.Error()
	}
	if derr := setCommentDisposition(ctx, d.Pool, c.ID, disp, detail); derr != nil {
		log.Printf("pipeline: comment %d disposition: %v", c.ID, derr)
	}
	if err != nil && !errors.Is(err, ErrNotApplicable) {
		return disp, err
	}
	return disp, nil
}

// ---- auth ----

type collabEntry struct {
	allowed bool
	at      time.Time
}

// commentAuthorizer resolves commenter authority: an explicit allowlist
// first, else repo-collaborator checks via gh, cached per login (MAQ-12
// point 4 + rate-limit safety).
type commentAuthorizer struct {
	cfg    GhCommandsConfig
	ttl    time.Duration
	mu     sync.Mutex
	cached map[string]collabEntry
}

func newCommentAuthorizer(cfg GhCommandsConfig) *commentAuthorizer {
	return &commentAuthorizer{cfg: cfg, ttl: DefaultGhAuthCacheTTL, cached: map[string]collabEntry{}}
}

// allowed reports whether login may issue commands. error = transient
// (GitHub unreachable): the caller retries the comment next pass.
func (a *commentAuthorizer) allowed(ctx context.Context, d CommentDeps, login string) (bool, error) {
	if len(a.cfg.AllowedLogins) > 0 {
		for _, l := range a.cfg.AllowedLogins {
			if strings.EqualFold(l, login) {
				return true, nil
			}
		}
		return false, nil
	}
	a.mu.Lock()
	e, ok := a.cached[login]
	a.mu.Unlock()
	if ok && time.Since(e.at) < a.ttl {
		return e.allowed, nil
	}
	if d.Source == nil {
		return false, fmt.Errorf("pipeline: no allowlist and no CommentSource for collaborator check")
	}
	allowed, err := d.Source.IsCollaborator(ctx, login)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.cached[login] = collabEntry{allowed: allowed, at: time.Now()}
	a.mu.Unlock()
	return allowed, nil
}

// authorizeCommenter is the nil-tolerant form used by the dispatcher.
func authorizeCommenter(ctx context.Context, d CommentDeps, authz *commentAuthorizer, login string) (bool, error) {
	if authz == nil {
		authz = newCommentAuthorizer(d.Auth)
	}
	return authz.allowed(ctx, d, login)
}

// ---- target resolution (id-less) ----

// resolveTaskByPR maps a PR number to its task with NO ids in the command:
// primary via tasks.pr_url (the PR maps 1:1 to the task), fallback via the
// merge_queue branch matching the PR head branch. errNoTaskForPR when
// nothing matches (the dispatcher records one clean no-op).
func resolveTaskByPR(ctx context.Context, pool *pgxpool.Pool, src CommentSource, pr int) (taskID string, err error) {
	num := strconv.Itoa(pr)
	qerr := pool.QueryRow(ctx, `
		SELECT id FROM tasks
		WHERE  pr_url LIKE '%/pull/' || $1
		LIMIT 1
	`, num).Scan(&taskID)
	if qerr == nil {
		return taskID, nil
	}
	if !errors.Is(qerr, pgx.ErrNoRows) {
		return "", fmt.Errorf("pipeline: resolving task for PR #%d: %w", pr, qerr)
	}
	// Branch fallback: the merge_queue entry knows the task's branch; the
	// PR head branch names the same line of work.
	if src != nil {
		branch, berr := src.PRHeadBranch(ctx, pr)
		if berr != nil {
			log.Printf("pipeline: PR #%d head branch (resolution fallback): %v", pr, berr)
		} else if branch != "" {
			ferr := pool.QueryRow(ctx, `
				SELECT t.id
				FROM   merge_queue q JOIN tasks t ON t.id = q.task_id
				WHERE  q.branch = $1
				ORDER  BY q.id DESC
				LIMIT 1
			`, branch).Scan(&taskID)
			if ferr == nil {
				return taskID, nil
			}
			if !errors.Is(ferr, pgx.ErrNoRows) {
				return "", fmt.Errorf("pipeline: resolving task for PR #%d by branch: %w", pr, ferr)
			}
		}
	}
	return "", errNoTaskForPR
}

// ---- exactly-once claim ----

// claimCommentCommand records the processed-comment memory: the INSERT
// (PK = GitHub comment id, ON CONFLICT DO NOTHING) IS the claim. Returns
// false when the comment was already processed.
func claimCommentCommand(ctx context.Context, pool *pgxpool.Pool, c PRComment, verb, taskID string) (bool, error) {
	tag, err := pool.Exec(ctx, `
		INSERT INTO gh_comment_commands (comment_id, verb, task_id, actor, disposition)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)
		ON CONFLICT (comment_id) DO NOTHING
	`, c.ID, verb, taskID, c.Author, DispPending)
	if err != nil {
		return false, fmt.Errorf("pipeline: claiming comment %d: %w", c.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func setCommentDisposition(ctx context.Context, pool *pgxpool.Pool, commentID int64, disposition, detail string) error {
	_, err := pool.Exec(ctx, `
		UPDATE gh_comment_commands SET disposition = $2, detail = $3
		WHERE comment_id = $1
	`, commentID, disposition, detail)
	if err != nil {
		return fmt.Errorf("pipeline: comment %d disposition: %w", commentID, err)
	}
	return nil
}

// ---- poller ----

// watchedPR is a PR whose conversation is polled: any open pipeline PR —
// the states the verbs and the MAQ-30 comment trigger act on. Comments on
// PRs outside the set are never fetched — the cheapest possible no-op.
func watchedPRs(ctx context.Context, pool *pgxpool.Pool) ([]int, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT pr_url
		FROM   tasks
		WHERE  pr_url IS NOT NULL
		  AND  status IN ('review', 'changes_requested', 'ready_to_merge', 'pending_approval')
	`)
	if err != nil {
		return nil, fmt.Errorf("pipeline: watching PRs: %w", err)
	}
	defer rows.Close()

	seen := map[int]bool{}
	var prs []int
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, fmt.Errorf("pipeline: watching PRs scan: %w", err)
		}
		pr, err := prFromPullURL(url)
		if err != nil {
			continue // not a GitHub pull URL — not ours to poll
		}
		if !seen[pr] {
			seen[pr] = true
			prs = append(prs, pr)
		}
	}
	return prs, rows.Err()
}

// PollPRCommands runs one pass: fetch each watched PR's comments since,
// dispatch commands, return the advanced cursor. The cursor only advances
// on a clean pass — a failed fetch OR a failed dispatch re-reads the whole
// window next pass, which the claims make exactly-once (a transient
// dispatch error claims nothing, so the retry re-runs the comment).
func PollPRCommands(ctx context.Context, d CommentDeps, authz *commentAuthorizer, since time.Time) (time.Time, error) {
	prs, err := watchedPRs(ctx, d.Pool)
	if err != nil {
		return since, err
	}
	cursor := since
	var passErr error
	hold := func(err error) {
		if passErr == nil {
			passErr = err
		}
	}
	for _, pr := range prs {
		comments, err := d.Source.PRComments(ctx, pr, since)
		if err != nil {
			log.Printf("pipeline: comments PR #%d: %v", pr, err)
			hold(fmt.Errorf("comments PR #%d: %w", pr, err))
			continue
		}
		sort.Slice(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
		for _, c := range comments {
			if !c.CreatedAt.After(since) {
				continue // exact created_at filter (the API filters on updated)
			}
			if c.CreatedAt.After(cursor) {
				cursor = c.CreatedAt
			}
			disp, err := DispatchCommentCommand(ctx, d, authz, pr, c)
			switch {
			case err != nil:
				log.Printf("pipeline: comment %d on PR #%d by @%s: dispatch error: %v", c.ID, pr, c.Author, err)
				// Dispatch errors hold the cursor (same contract as fetch
				// errors): a transient `("", err)` claimed nothing, so the
				// comment must be re-read next pass or it is silently
				// swallowed. A claimed one re-reads as a harmless duplicate.
				hold(fmt.Errorf("comment %d on PR #%d: %w", c.ID, pr, err))
			case disp == "":
				// defensive: the arms always record a disposition
			default:
				log.Printf("pipeline: comment %d on PR #%d by @%s → %s", c.ID, pr, c.Author, disp)
			}
		}
	}
	if passErr != nil {
		return since, passErr
	}
	return cursor, nil
}

// RunCommentCommands polls PR conversations for `maquinista <verb>`
// comments and dispatches them until ctx is cancelled. gh merge mode only —
// the verbs drive the PR merge flow (wired in orchestrator start).
func RunCommentCommands(ctx context.Context, d CommentDeps) {
	interval := d.Auth.Interval
	if interval <= 0 {
		interval = DefaultGhCommentsPoll
	}
	catchup := d.Auth.Catchup
	if catchup <= 0 {
		catchup = DefaultGhCommentsCatchup
	}
	authz := newCommentAuthorizer(d.Auth)
	cursor := time.Now().UTC().Add(-catchup)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next, err := PollPRCommands(ctx, d, authz, cursor)
			if err != nil {
				log.Printf("pipeline: comment commands pass: %v", err)
				continue // cursor unchanged: the window is re-read next pass
			}
			cursor = next
		}
	}
}
