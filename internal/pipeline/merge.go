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
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"

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

// GhRunner abstracts the GitHub side (gh CLI in production, a fake in
// tests). Kept minimal: one read, one write.
type GhRunner interface {
	// PRChecks returns the aggregate state of the PR's CI checks.
	PRChecks(ctx context.Context, pr int) (string, error)
	// PRMergeSquash squash-merges the PR via the API/CLI.
	PRMergeSquash(ctx context.Context, pr int) error
}

// MergeConfig carries the merge-mode knobs plus the GhRunner. Gh is nil in
// local mode; the wiring layer injects the real runner (gh binary).
type MergeConfig struct {
	Mode      string
	AutoMerge bool // false: entries wait for the approve verb even in gh mode
	Gh        GhRunner
}

// MergeConfigFromEnv reads PIPELINE_MERGE_MODE ("local"|"gh") and
// PIPELINE_AUTO_MERGE ("0"/"1", default 0). The GhRunner is wired by the
// caller — env only selects behavior, never binaries.
func MergeConfigFromEnv() MergeConfig {
	cfg := MergeConfig{Mode: MergeModeLocal}
	if v := os.Getenv("PIPELINE_MERGE_MODE"); v == MergeModeGH {
		cfg.Mode = MergeModeGH
	}
	switch os.Getenv("PIPELINE_AUTO_MERGE") {
	case "1", "true", "TRUE":
		cfg.AutoMerge = true
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
	PRURL        string
	WorktreePath string
	IssueID      string
}

func loadTaskMergeInfo(pool *pgxpool.Pool, taskID string) (*taskMergeInfo, error) {
	var prURL, worktree *string
	var metadata []byte
	err := pool.QueryRow(context.Background(), `
		SELECT pr_url, worktree_path, metadata FROM tasks WHERE id = $1
	`, taskID).Scan(&prURL, &worktree, &metadata)
	if err != nil {
		return nil, fmt.Errorf("pipeline: loading merge info for %s: %w", taskID, err)
	}
	info := &taskMergeInfo{}
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
	pr, err := prFromPullURL(info.PRURL)
	if err != nil {
		return err
	}
	if info.WorktreePath == "" {
		return fmt.Errorf("pipeline: task %s has no worktree to merge from", taskID)
	}
	wt := info.WorktreePath

	// 1. Sync the branch with the remote and rebase onto origin's default.
	base, err := defaultBranch(wt)
	if err != nil {
		base = entry.BaseBranch
	}
	if err := git.Fetch(wt, "origin"); err != nil {
		return failMerge(pool, entry.ID, taskID, fmt.Sprintf("git fetch failed: %v", err))
	}
	if _, err := git.Rebase(wt, "origin/"+base); err != nil {
		if conflictErr, ok := err.(*git.ConflictError); ok {
			return parkMergeConflict(ctx, pool, taskID, entry, conflictErr)
		}
		return failMerge(pool, entry.ID, taskID, fmt.Sprintf("rebase onto %s failed: %v", base, err))
	}

	// 2. Publish the rebased branch (lease-guarded force push).
	if err := git.PushForceWithLease(wt, entry.Branch, "origin"); err != nil {
		return failMerge(pool, entry.ID, taskID, fmt.Sprintf("push failed: %v", err))
	}

	// 3. CI gate: pending → release for a later pass; failed → park needs-human.
	checks, err := cfg.Gh.PRChecks(ctx, pr)
	if err != nil {
		return failMerge(pool, entry.ID, taskID, fmt.Sprintf("gh pr checks failed: %v", err))
	}
	switch checks {
	case ChecksPending:
		if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
			return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
		}
		log.Printf("pipeline: merge %s waiting for CI on PR #%d", taskID, pr)
		return nil
	case ChecksFailed:
		db.AddObservation(pool, taskID, "merger",
			fmt.Sprintf("CI failed on PR #%d — merge deferred; fix and re-push.", pr))
		if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
			return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
		}
		log.Printf("pipeline: merge %s CI failed on PR #%d", taskID, pr)
		return nil
	default:
		// green / none — proceed.
	}

	// 4. Squash-merge on the remote.
	if err := cfg.Gh.PRMergeSquash(ctx, pr); err != nil {
		return failMerge(pool, entry.ID, taskID, fmt.Sprintf("gh pr merge failed: %v", err))
	}

	// 5. Record the squash commit SHA (the merged tip of the default branch).
	git.Fetch(wt, "origin") // best-effort; SHA comes back empty on failure
	mergeSHA, _ := git.RevParse(wt, "origin/"+base)

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
	log.Printf("pipeline: merge %s conflict: %v", taskID, conflictErr)
	return nil
}

// failMerge records an infrastructure failure on the queue entry and leaves
// the task in ready_to_merge (the work is done; only the merge machinery
// failed, so a retry can succeed).
func failMerge(pool *pgxpool.Pool, entryID int64, taskID, msg string) error {
	if err := db.FailMerge(pool, entryID, msg); err != nil {
		return fmt.Errorf("pipeline: failing merge %d: %w", entryID, err)
	}
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
