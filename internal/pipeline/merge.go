package pipeline

// GitHub merge mode (ADR appendix EX-05). The determinism boundary extends
// to merging: instead of local git merges, the processor drives the remote
// through a single choke point — the branch is rebased onto origin's
// default, force-pushed with lease, gated on CI, then squash-merged via
// `gh`. The merge_queue entry is the driver; task and board state move in
// the same steps the local flow uses, so the audit trail is identical.

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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
)

// Merge modes (PIPELINE_MERGE_MODE). "local" is the legacy MergeNoFF path;
// "gh" drives the remote PR via the GhRunner.
const (
	MergeModeLocal = "local"
	MergeModeGH    = "gh"
)

// Checks states returned by GhRunner.PRChecks.
const (
	ChecksGreen   = "green"
	ChecksPending = "pending"
	ChecksFailed  = "failed"
	ChecksNone    = "none" // no checks configured — vacuously green
)

// ErrMergeUpConflict: the PR's base cannot be merged into the branch
// cleanly — GitHub reports HTTP 422 on the update-branch endpoint (MAQ-26).
// A deterministic conflict, not infrastructure: the local merge-up fallback
// would conflict on the same three-way inputs, so it is skipped.
var ErrMergeUpConflict = errors.New("merge-up conflict")

// ErrMergeUpRace: the branch moved since the merge-up's expected head SHA
// (HTTP 409) — e.g. a fixer pushed mid-flight. State changed underneath, so
// the entry is released for a fresh pass instead of consuming budget.
var ErrMergeUpRace = errors.New("merge-up race: branch moved")

// GhRunner abstracts the GitHub side (gh CLI in production, a fake in
// tests). Kept minimal: two reads, two writes (MAQ-16 added the PR-comment
// pair — verdict posts + human-comment reads; MAQ-26 added the branch
// update for the auto merge-up).
type GhRunner interface {
	// PRChecks returns the aggregate state of the PR's CI checks.
	PRChecks(ctx context.Context, pr int) (string, error)
	// PRMergeSquash squash-merges the PR via the API/CLI.
	PRMergeSquash(ctx context.Context, pr int) error
	// PRComments lists the PR's issue comments created after since (zero
	// since = all), oldest first. Same method serves the CommentSource
	// poller (since-cursored) and the reviewer-prompt reader (zero since).
	PRComments(ctx context.Context, pr int, since time.Time) ([]PRComment, error)
	// PRPostComment posts body as a new comment on the PR.
	PRPostComment(ctx context.Context, pr int, body string) error
	// PRUpdateBranch merges the PR's base branch into the PR branch (the
	// "Update branch" button; MAQ-26). expectedHeadSHA is the branch tip the
	// caller observed — GitHub rejects with HTTP 409 when the branch has
	// moved (mapped to ErrMergeUpRace). A 422 — base cannot merge cleanly —
	// maps to ErrMergeUpConflict.
	PRUpdateBranch(ctx context.Context, pr int, expectedHeadSHA string) error
}

// MergeConfig carries the merge-mode knobs plus the GhRunner. Gh is nil in
// local mode; the wiring layer injects the real runner (gh binary).
type MergeConfig struct {
	Mode      string
	AutoMerge bool // false: entries wait for the approve verb even in gh mode
	// MergeAgent arms the merger-agent conflict leg (MAQ-15): a rebase
	// conflict parks an episode marker and releases the entry for the
	// dispatch loop's merger agent instead of parking needs-human
	// immediately. false (default): conflicts park needs-human as before.
	MergeAgent bool
	// MaxAttempts caps how many times an auto-merged entry may reclaim a
	// red PR before the task is parked needs-human (EX-06: the release-
	// and-reclaim loop otherwise spams forever). 0 → default 5. The merger
	// conflict leg consumes the SAME budget (MAQ-15): each armed merger
	// episode is one attempt.
	MaxAttempts int
	Gh          GhRunner
}

// defaultMergeAttempts is the CI retry cap when MAQUINISTA_MERGE_ATTEMPTS_MAX
// is unset. A cap of 5 gives a flaky PR room to recover while bounding the
// churn.
const defaultMergeAttempts = 5

// truthyEnv is the accepted spelling set for boolean env knobs (EX-06: the
// reviewer nit — AUTO_MERGE used to accept only 1/TRUE spellings).
var truthyEnv = map[string]bool{
	"1": true, "t": true, "true": true, "y": true, "yes": true,
}

// MergeConfigFromEnv reads PIPELINE_MERGE_MODE ("local"|"gh"),
// PIPELINE_AUTO_MERGE (1/true/yes…, default 0), PIPELINE_MERGE_AGENT
// (truthy → merger-agent conflict leg, default 0) and
// MAQUINISTA_MERGE_ATTEMPTS_MAX (default 5). The GhRunner is wired by the
// caller — env only selects behavior, never binaries.
func MergeConfigFromEnv() MergeConfig {
	cfg := MergeConfig{Mode: MergeModeLocal, MaxAttempts: defaultMergeAttempts}
	if v := os.Getenv("PIPELINE_MERGE_MODE"); v == MergeModeGH {
		cfg.Mode = MergeModeGH
	}
	if truthyEnv[strings.ToLower(os.Getenv("PIPELINE_AUTO_MERGE"))] {
		cfg.AutoMerge = true
	}
	if truthyEnv[strings.ToLower(os.Getenv("PIPELINE_MERGE_AGENT"))] {
		cfg.MergeAgent = true
	}
	if v := os.Getenv("MAQUINISTA_MERGE_ATTEMPTS_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxAttempts = n
		} else {
			log.Printf("pipeline: invalid MAQUINISTA_MERGE_ATTEMPTS_MAX %q — using %d", v, cfg.MaxAttempts)
		}
	}
	return cfg
}

var prURLNum = regexp.MustCompile(`/pull/(\d+)/?$`)

// prFromPullURL extracts the PR number from a GitHub pull URL.
func prFromPullURL(url string) (int, error) {
	m := prURLNum.FindStringSubmatch(url)
	if m == nil {
		return 0, fmt.Errorf("pipeline: not a GitHub pull URL: %q", url)
	}
	return strconv.Atoi(m[1])
}

// taskMergeInfo is the slice of the task row the merge flow needs (the
// shared Task struct does not carry the pipeline columns).
type taskMergeInfo struct {
	Status       string
	PRURL        string
	WorktreePath string
	IssueID      string
}

func loadTaskMergeInfo(pool *pgxpool.Pool, taskID string) (*taskMergeInfo, error) {
	var status string
	var prURL, worktree *string
	var metadata []byte
	err := pool.QueryRow(context.Background(), `
		SELECT status, pr_url, worktree_path, metadata FROM tasks WHERE id = $1
	`, taskID).Scan(&status, &prURL, &worktree, &metadata)
	if err != nil {
		return nil, fmt.Errorf("pipeline: loading merge info for %s: %w", taskID, err)
	}
	info := &taskMergeInfo{Status: status}
	if prURL != nil {
		info.PRURL = *prURL
	}
	if worktree != nil {
		info.WorktreePath = *worktree
	}
	if len(metadata) > 0 {
		var md map[string]any
		if err := json.Unmarshal(metadata, &md); err == nil {
			if id, ok := md["ticket_issue_id"].(string); ok {
				info.IssueID = id
			}
		}
	}
	return info, nil
}

// RunMergeEnqueuePass enqueues merge_queue entries for pipeline tasks that
// landed in ready_to_merge with a PR URL and a worktree, but have no live
// queue entry yet. Returns the number of entries created. No-op in local
// mode (the legacy engine owns local merges).
func RunMergeEnqueuePass(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig) (int, error) {
	if cfg.Mode != MergeModeGH {
		return 0, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT t.id, COALESCE(t.worktree_path, '')
		FROM   tasks t
		WHERE  t.status = 'ready_to_merge'
		  AND  t.pr_url IS NOT NULL
		  AND  t.worktree_path IS NOT NULL
		  AND  NOT EXISTS (
		       SELECT 1 FROM merge_queue q
		       WHERE  q.task_id = t.id
		         AND  q.status IN ('pending','merging'))
	`)
	if err != nil {
		return 0, fmt.Errorf("pipeline: merge enqueue pass: %w", err)
	}
	defer rows.Close()

	type candidate struct{ id, worktree string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.worktree); err != nil {
			return 0, fmt.Errorf("pipeline: merge enqueue scan: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pipeline: merge enqueue rows: %w", err)
	}

	enqueued := 0
	for _, c := range candidates {
		branch, err := git.CurrentBranch(c.worktree)
		if err != nil {
			log.Printf("pipeline: merge enqueue %s: worktree %s unreadable: %v", c.id, c.worktree, err)
			continue
		}
		base, err := defaultBranch(c.worktree)
		if err != nil {
			base = "main"
		}
		if err := db.EnqueueMerge(pool, c.id, "merger", branch, c.worktree, base, ""); err != nil {
			return enqueued, fmt.Errorf("pipeline: merge enqueue %s: %w", c.id, err)
		}
		enqueued++
	}
	return enqueued, nil
}

// defaultBranch resolves the remote default branch (origin/HEAD).
func defaultBranch(worktree string) (string, error) {
	return git.SymbolicRefRemoteHead(worktree, "origin")
}

// mergeEnqueuePass is the dispatch-loop arm of the gh merge flow: enqueue
// merge_queue entries for ready_to_merge tasks (no-op outside gh mode).
func mergeEnqueuePass(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := RunMergeEnqueuePass(ctx, pool, MergeConfigFromEnv())
	return err
}

// ProcessMergeGH runs one merge attempt for a claimed queue entry in gh
// mode. Every terminal outcome records queue + task + observation state and
// returns nil; only infrastructure errors return an error.
func ProcessMergeGH(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, prov TicketProvider, teamID string, entry *db.MergeQueueEntry) error {
	taskID := entry.TaskID

	// Human gate: without auto-merge the entry waits for the approve verb.
	if !cfg.AutoMerge {
		if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
			return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
		}
		log.Printf("pipeline: merge %s waiting for approval (auto-merge off)", taskID)
		return nil
	}

	if cfg.Gh == nil {
		return fmt.Errorf("pipeline: gh mode requires a GhRunner")
	}

	info, err := loadTaskMergeInfo(pool, taskID)
	if err != nil {
		return err
	}
	// Status guard: only merge ready_to_merge tasks. The merger episode
	// flips the task to pending_approval atomically with its queue-entry
	// landing — a processor that claimed the entry just before that must
	// release, not merge a parked task behind a needs-human verdict.
	if info.Status != "ready_to_merge" {
		if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
			return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
		}
		log.Printf("pipeline: merge %s skipped — task status %q (entry released)", taskID, info.Status)
		return nil
	}
	// Merger-episode guard (MAQ-15): while a merger agent is resolving a
	// conflict — live pane, or an armed marker no verdict consumed yet —
	// the entry is released untouched. Rebasing a worktree the merger is
	// mid-resolution in would corrupt the episode; re-processing would
	// double-arm it.
	if cfg.MergeAgent {
		pending, err := mergerEpisodePending(ctx, pool, taskID)
		if err != nil {
			log.Printf("pipeline: merge %s: merger episode check: %v", taskID, err)
		}
		if pending {
			if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
				return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
			}
			log.Printf("pipeline: merge %s: merger episode in flight — entry released for a later pass", taskID)
			return nil
		}
	}
	pr, err := prFromPullURL(info.PRURL)
	if err != nil {
		// Bad/unset PR URL: the work is done but the merge machinery
		// cannot run — record failed so the queue shows it instead of
		// wedging the entry in 'merging'.
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("cannot resolve PR: %v", err))
	}
	if info.WorktreePath == "" {
		return failMerge(ctx, pool, entry.ID, taskID, "task has no worktree to merge from")
	}
	wt := info.WorktreePath

	// MAQ-25: the merge leg's pickup marker on the PR — the gate is about
	// to run for real (human gate, status guard and merger-episode guard
	// all passed). Best effort, deduped once per PR by needle scan: the
	// release-and-reclaim loop re-runs this pass per attempt.
	postPickupComment(ctx, pool, cfg.Gh, taskID, mergePickupNeedle, mergePickupBody(taskIssueKey(ctx, pool, taskID)))

	// 1. Sync the branch with the remote and fold the base in.
	base, err := defaultBranch(wt)
	if err != nil {
		base = entry.BaseBranch
	}
	if err := git.Fetch(wt, "origin"); err != nil {
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("git fetch failed: %v", err))
	}
	// Up-to-date fast path (MAQ-26): the remote branch already contains the
	// base — a prior pass's clean rebase-push, or an auto merge-up waiting
	// out its CI. Skip the rebase: it could only re-conflict (or silently
	// drop the merge-up's merge commit) and re-push what is already on the
	// remote. Everything downstream reads origin/<branch>, so what is gated
	// and merged is exactly what GitHub holds. A branch never pushed at all
	// (approve-verb paths work off a local-only branch) has nothing to
	// fast-path on — the rebase leg runs and the push creates the ref.
	upToDate := false
	if exists, err := git.RefExists(wt, "origin/"+entry.Branch); err != nil {
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("mergeability check failed: %v", err))
	} else if exists {
		upToDate, err = git.IsAncestor(wt, "origin/"+base, "origin/"+entry.Branch)
		if err != nil {
			return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("mergeability check failed: %v", err))
		}
	}
	if !upToDate {
		if _, err := git.Rebase(wt, "origin/"+base); err != nil {
			conflictErr, ok := err.(*git.ConflictError)
			if !ok {
				return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("rebase onto %s failed: %v", base, err))
			}
			// MAQ-15: under PIPELINE_MERGE_AGENT the conflict arms a merger
			// episode (marker + released entry; the dispatch loop spawns the
			// agent) instead of parking needs-human immediately.
			if cfg.MergeAgent {
				return armMergeConflictAgent(ctx, pool, cfg, entry, base, conflictErr)
			}
			// MAQ-26: otherwise attempt an automatic merge-up before any
			// human gets pinged — a stale branch that merges cleanly heals
			// here, on the branch ref, never in the task worktree.
			return mergeUpAfterConflict(ctx, pool, cfg, prov, teamID, entry, info, wt, base, pr, conflictErr)
		}

		// 2. Publish the rebased branch (lease-guarded force push).
		if err := git.PushForceWithLease(wt, entry.Branch, "origin"); err != nil {
			return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("push failed: %v", err))
		}
	}

	// 3–7. Gates, squash, bookkeeping, cleanup.
	return finishMergeGH(ctx, pool, cfg, prov, teamID, entry, info, wt, base, pr)
}

// finishMergeGH runs steps 3–7 of the gh merge flow (CI gate → build gate →
// squash-merge → bookkeeping → board sync → cleanup) for a branch whose
// remote tip already contains the base: either the caller's clean rebase +
// lease push, or an auto merge-up (MAQ-26) that folded the base in on the
// ref. Every git read targets origin/<branch> — the remote state — never
// the task worktree copy, which the merge-up path deliberately leaves
// untouched (a fixer may be working there).
func finishMergeGH(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, prov TicketProvider, teamID string, entry *db.MergeQueueEntry, info *taskMergeInfo, wt, base string, pr int) error {
	taskID := entry.TaskID

	// 3. CI gate: pending → release for a later pass; failed → park needs-human.
	checks, err := cfg.Gh.PRChecks(ctx, pr)
	if err != nil {
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("gh pr checks failed: %v", err))
	}
	// Normalize the cap: struct-literal callers (tests, RunMergeOnApprove)
	// leave MaxAttempts at the zero value, which must mean the default —
	// a 0 cap would park a red PR on its first failure.
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMergeAttempts
	}
	switch checks {
	case ChecksPending:
		// CI still running: release for a later pass; the worker keeps
		// pushing (fail-forward).
		db.AddObservation(pool, taskID, "merger",
			fmt.Sprintf("PR #%d checks still running — merge deferred; work continues.", pr))
		if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
			return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
		}
		log.Printf("pipeline: merge %s waiting for CI on PR #%d", taskID, pr)
		return nil
	case ChecksFailed:
		// Cap the reclaim loop (EX-06): a red PR under AUTO_MERGE=1
		// otherwise releases and re-claims forever. Below cap: silent
		// release (the spam bug the cap exists for). At cap: entry
		// failed, task parked needs-human, one question out.
		attempts, err := db.BumpMergeAttempts(pool, entry.ID)
		if err != nil {
			return fmt.Errorf("pipeline: bumping attempts %d: %w", entry.ID, err)
		}
		if attempts < maxAttempts {
			if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
				return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
			}
			log.Printf("pipeline: merge %s CI failed on PR #%d (attempt %d/%d) — will re-check", taskID, pr, attempts, cfg.MaxAttempts)
			return nil
		}
		if err := db.FailMerge(pool, entry.ID, fmt.Sprintf("CI failed %d times on PR #%d", attempts, pr)); err != nil {
			return fmt.Errorf("pipeline: failing %d: %w", entry.ID, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE tasks
			SET    status = 'pending_approval'
			WHERE  id = $1 AND status = 'ready_to_merge'
		`, taskID); err != nil {
			return fmt.Errorf("pipeline: parking %s after CI cap: %w", taskID, err)
		}
		db.AddObservation(pool, taskID, "merger",
			fmt.Sprintf("CI failed %d times on PR #%d — parked needs-human.", attempts, pr))
		notifyTaskf(ctx, pool, taskID, "🆘 %s: CI failed %d times on PR #%d — parked needs-human. Fix, re-push, then `maquinista approve %s` to retry the merge.%s",
			taskTitle(ctx, pool, taskID), attempts, pr, taskID, prLinkSuffix(ctx, pool, taskID))
		log.Printf("pipeline: merge %s CI failed %d times on PR #%d — parked needs-human", taskID, attempts, pr)
		return nil
	default:
		// green / none — proceed.
	}

	// 3.5 Build gate (MAQ-20): compile the branch before the squash. CI
	// does not run on PRs, so this is the only point between the implementor
	// verdict and the merge that executes a build — PR #21 merged duplicate
	// consts and broke main because nothing here compiled. origin/<branch>
	// is the tree a squash-merge takes: step 1's rebase already folded in
	// the latest base and step 2 lease-pushed it, so the gate never builds
	// a stale tree.
	passed, buildOut, err := runBuildGate(ctx, wt, "origin/"+entry.Branch)
	if err != nil {
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("build gate could not run: %v", err))
	}
	if !passed {
		return parkBuildFailure(ctx, pool, taskID, entry, buildOut)
	}

	// 4. Squash-merge on the remote.
	if err := cfg.Gh.PRMergeSquash(ctx, pr); err != nil {
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("gh pr merge failed: %v", err))
	}

	// 5. Record the squash commit SHA (the merged tip of the default branch).
	mergeSHA := ""
	if err := git.Fetch(wt, "origin"); err != nil {
		log.Printf("pipeline: merge %s post-merge fetch failed, no SHA recorded: %v", taskID, err)
	} else if sha, err := git.RevParse(wt, "origin/"+base); err == nil {
		mergeSHA = sha
	}

	if err := db.CompleteMerge(pool, entry.ID, mergeSHA); err != nil {
		return fmt.Errorf("pipeline: completing merge %d: %w", entry.ID, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		SET    status = 'done', pr_state = 'merged'
		WHERE  id = $1 AND status = 'ready_to_merge'
	`, taskID); err != nil {
		return fmt.Errorf("pipeline: marking %s merged: %w", taskID, err)
	}
	db.AddObservation(pool, taskID, "merger",
		fmt.Sprintf("PR #%d squash-merged into %s (%s).", pr, base, mergeSHA))
	notifyTaskf(ctx, pool, taskID, "✅ %s merged: PR #%d squash-merged into %s (%s).%s",
		taskTitle(ctx, pool, taskID), pr, base, mergeSHA, prLinkSuffix(ctx, pool, taskID))

	// 6. Board sync (best-effort — the sync loop self-heals on next tick).
	if prov != nil && info.IssueID != "" {
		if cols, err := prov.Columns(ctx, teamID); err == nil {
			if colID, ok := cols[ColDone]; ok {
				if err := prov.SetIssueColumn(ctx, info.IssueID, colID); err != nil {
					log.Printf("pipeline: merge %s board sync failed: %v", taskID, err)
				}
			}
		} else {
			log.Printf("pipeline: merge %s board columns unavailable: %v", taskID, err)
		}
	}

	// 7. Cleanup: remove the task worktree + local and remote branches.
	admin, err := git.CommonDir(wt)
	if err == nil {
		if err := git.DeleteRemoteBranch(admin, entry.Branch, "origin"); err != nil {
			log.Printf("pipeline: merge %s remote branch cleanup: %v", taskID, err)
		}
		if err := git.WorktreeRemove(admin, wt); err != nil {
			log.Printf("pipeline: merge %s worktree cleanup: %v", taskID, err)
		}
		if err := git.DeleteBranch(admin, entry.Branch); err != nil {
			log.Printf("pipeline: merge %s branch cleanup: %v", taskID, err)
		}
	} else {
		log.Printf("pipeline: merge %s cleanup skipped (repo root unreadable): %v", taskID, err)
	}

	log.Printf("pipeline: merge %s merged PR #%d (%s)", taskID, pr, mergeSHA)
	return nil
}

// parkMergeConflict records a rebase conflict: queue entry → conflict, task
// → pending_approval (a human untangles conflicts), observation with the
// conflicting files.
func parkMergeConflict(ctx context.Context, pool *pgxpool.Pool, taskID string, entry *db.MergeQueueEntry, conflictErr *git.ConflictError) error {
	if err := db.ConflictMerge(pool, entry.ID, conflictErr.Files); err != nil {
		return fmt.Errorf("pipeline: recording conflict %d: %w", entry.ID, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		SET    status = 'pending_approval'
		WHERE  id = $1 AND status = 'ready_to_merge'
	`, taskID); err != nil {
		return fmt.Errorf("pipeline: parking %s needs-human: %w", taskID, err)
	}
	db.AddObservation(pool, taskID, "merger",
		fmt.Sprintf("Rebase conflict on branch %s: %s. Resolving requires human judgment.",
			entry.Branch, conflictErr.Error()))
	// EX-06: the needs-human question carries the conflict files so the
	// human can decide without opening the worktree. (Conflict →
	// merger-agent resolution stays deferred; the plan records why.)
	notifyTaskf(ctx, pool, taskID, "🆘 %s: rebase conflict on branch %s. Conflicting files:\n%s\nTask parked needs-human.%s",
		taskTitle(ctx, pool, taskID), entry.Branch, strings.Join(conflictErr.Files, "\n"), prLinkSuffix(ctx, pool, taskID))
	log.Printf("pipeline: merge %s conflict: %v", taskID, conflictErr)
	return nil
}

// failMerge records an infrastructure failure on the queue entry and leaves
// the task in ready_to_merge (the work is done; only the merge machinery
// failed, so a retry can succeed). The entry is terminal — the queue will
// NOT retry it — so a human gets a note (EX-06); re-approving re-enqueues.
func failMerge(ctx context.Context, pool *pgxpool.Pool, entryID int64, taskID, msg string) error {
	if err := db.FailMerge(pool, entryID, msg); err != nil {
		return fmt.Errorf("pipeline: failing merge %d: %w", entryID, err)
	}
	notifyTaskf(ctx, pool, taskID, "⚠️ %s: merge failed — %s. The queue entry is failed; reply `approve %s` here (or comment `approve` on the ticket issue) to re-enqueue the merge.%s",
		taskTitle(ctx, pool, taskID), msg, shortTaskID(taskID), prLinkSuffix(ctx, pool, taskID))
	log.Printf("pipeline: merge %s failed: %s", taskID, msg)
	return nil
}

// RunMergeOnApprove is the approve verb's gh-mode arm: merge one
// ready_to_merge task now, bypassing the auto-merge gate. The entry is
// enqueued on demand if the pass hasn't created one yet.
func RunMergeOnApprove(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, prov TicketProvider, teamID string, taskID string) error {
	entry, err := db.GetPendingMergeEntryByTask(pool, taskID)
	if err != nil {
		return err
	}
	if entry == nil {
		info, err := loadTaskMergeInfo(pool, taskID)
		if err != nil {
			return err
		}
		if info.WorktreePath == "" {
			return fmt.Errorf("merge: task %s has no worktree", taskID)
		}
		branch, err := git.CurrentBranch(info.WorktreePath)
		if err != nil {
			return fmt.Errorf("merge: reading branch of %s: %w", info.WorktreePath, err)
		}
		base, err := defaultBranch(info.WorktreePath)
		if err != nil {
			base = "main"
		}
		if err := db.EnqueueMerge(pool, taskID, "merger", branch, info.WorktreePath, base, ""); err != nil {
			return err
		}
		if entry, err = db.GetPendingMergeEntryByTask(pool, taskID); err != nil {
			return err
		}
		if entry == nil {
			return fmt.Errorf("merge: entry for %s vanished after enqueue", taskID)
		}
	}

	// Human approval overrides the auto-merge gate for this one merge.
	approvedCfg := cfg
	approvedCfg.AutoMerge = true
	claimed, err := db.ClaimMergeEntryByID(pool, entry.ID)
	if err != nil {
		return err
	}
	if claimed == nil {
		return fmt.Errorf("merge: entry for %s is being processed elsewhere", taskID)
	}
	return ProcessMergeGH(ctx, pool, approvedCfg, prov, teamID, claimed)
}
