package pipeline

// Auto merge-up (MAQ-26). Before 03/10 a stale branch — main advanced under
// an open PR — parked needs-human on the FIRST rebase conflict, even when
// the overlap was trivial and a clean merge of the base into the branch
// would have healed it (#24/#27/#28 in one afternoon; #28 healed by hand
// with merge commit 28f6271). This file is the retry leg the watcher
// precedent asked for: on a rebase conflict (and no merger agent armed),
// fold the base into the BRANCH REF — GitHub's update-branch API first, a
// local ref merge in a throwaway detached worktree as fallback — re-check
// mergeability (origin/base must be an ancestor of origin/<branch>), and
// let the normal gates + squash proceed on the healed ref. Only N=2 FAILED
// merge-ups park needs-human: a conflict that survives two attempts is a
// semantic overlap (#24's 7 files), which is exactly what the park is for.
//
// Worktree discipline: the merge-up never touches the task worktree — a
// fixer may be mid-episode in it. The local fallback materializes
// origin/<branch> in its own detached worktree (the build-gate pattern) and
// publishes the merge commit as a strict fast-forward push; the API path is
// GitHub's own push. Either way the checked-out copy stays where it was.
//
// Budget: each failed attempt consumes one unit of merge_queue's
// mergeup_attempts (migration 040) — deliberately separate from `attempts`,
// the CI/merger reclaim budget. Below the cap the entry is released for a
// later pass (base may have moved again); at the cap the conflict parks
// needs-human exactly as before. Every failed attempt comments on the PR
// through the MAQ-16/#18 transport (GhRunner.PRPostComment) so the failure
// is visible where the work lives; the park keeps its Telegram question.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
)

const (
	// maxMergeUps is the failed-merge-up budget before a rebase conflict
	// parks needs-human (MAQ-26: "N=2"). One clean merge of the base heals
	// staleness; two consecutive failures mean the overlap is semantic and
	// no amount of retrying will merge it. Deliberately a constant, not a
	// config knob — a tunable parking cap would be argued down to zero.
	maxMergeUps = 2
	// mergeUpDirPrefix names the throwaway worktrees the local fallback
	// creates under os.TempDir() — also the leak-detection glob in tests.
	mergeUpDirPrefix = "maquinista-mergeup-"
)

// mergeUpAfterConflict is the no-merger-agent conflict leg of ProcessMergeGH.
// Attempts the automatic merge-up; on success re-checks mergeability and
// re-enters the normal gate → squash path; on a deterministic conflict
// consumes one attempt, comments on the PR, and either releases the entry
// for a later pass (below cap) or parks needs-human (at cap). Infrastructure
// trouble fails the entry non-parkingly (failMerge) — the work is done, only
// the machinery hiccupped, and a re-approve retries.
func mergeUpAfterConflict(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, prov TicketProvider, teamID string, entry *db.MergeQueueEntry, info *taskMergeInfo, wt, base string, pr int, conflictErr *git.ConflictError) error {
	taskID := entry.TaskID

	method, upErr := runMergeUp(ctx, cfg, wt, entry, pr, base)
	if upErr == nil {
		// Re-check mergeability before re-entering the gates: the healed
		// ref must actually contain the base now. This is the guard against
		// a claimed-but-unsynced API success or a lost push — it counts as
		// a failed attempt, never as a mergeable branch.
		if err := git.Fetch(wt, "origin"); err != nil {
			return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("merge-up verification fetch failed: %v", err))
		}
		mergeable, err := git.IsAncestor(wt, "origin/"+base, "origin/"+entry.Branch)
		if err != nil {
			return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("merge-up verification failed: %v", err))
		}
		if mergeable {
			db.AddObservation(pool, taskID, "merger",
				fmt.Sprintf("Auto merge-up healed stale branch %s: origin/%s folded in via %s; re-checked mergeable — proceeding to gates.",
					entry.Branch, base, method))
			notifyTaskf(ctx, pool, taskID, "🔀 %s: the branch was stale — an automatic merge-up (%s) folded origin/%s in; merge gates re-run. No action needed.",
				taskTitle(ctx, pool, taskID), method, base)
			log.Printf("pipeline: merge %s conflict healed by auto merge-up (%s) on branch %s", taskID, method, entry.Branch)
			return finishMergeGH(ctx, pool, cfg, prov, teamID, entry, info, wt, base, pr)
		}
		if !mergeable {
			// Claimed but unverifiable (stubbed API success, lost push): not
			// mergeable, not infrastructure — a failed attempt, handled
			// below with the deterministic conflicts.
			log.Printf("pipeline: merge %s merge-up claimed success but origin/%s still lacks origin/%s — counting as a failed attempt",
				taskID, entry.Branch, base)
			upErr = &git.ConflictError{}
		}
	}

	// A race (branch moved underneath the merge-up) changed the state the
	// conflict was diagnosed in — release for a fresh pass without
	// consuming budget; the next pass re-fetches and re-decides.
	if errors.Is(upErr, ErrMergeUpRace) {
		if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
			return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
		}
		log.Printf("pipeline: merge %s merge-up raced on branch %s — entry released for a fresh pass", taskID, entry.Branch)
		return nil
	}
	// Any other non-conflict failure is infrastructure (network, gh, push
	// transport): terminal entry, task stays re-approvable, human informed.
	var conflict *git.ConflictError
	if !errors.As(upErr, &conflict) {
		return failMerge(ctx, pool, entry.ID, taskID, fmt.Sprintf("auto merge-up failed: %v", upErr))
	}

	// Deterministic conflict. Report the merge-up's own file list when the
	// local fallback produced one; the API's 422 carries none, so fall back
	// to the files the rebase probe named (same three-way overlap).
	files := conflict.Files
	if len(files) == 0 {
		files = conflictErr.Files
	}
	attempts, err := db.BumpMergeUpAttempts(pool, entry.ID)
	if err != nil {
		return fmt.Errorf("pipeline: bumping merge-up attempts %d: %w", entry.ID, err)
	}
	postMergeUpComment(ctx, cfg.Gh, pr, mergeUpCommentBody(entry.Branch, base, files, attempts))
	log.Printf("pipeline: merge %s auto merge-up conflicted on branch %s (attempt %d/%d): %v",
		taskID, entry.Branch, attempts, maxMergeUps, conflict)

	if attempts >= maxMergeUps {
		return parkMergeConflict(ctx, pool, taskID, entry, &git.ConflictError{Files: files})
	}
	if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
		return fmt.Errorf("pipeline: releasing %d: %w", entry.ID, err)
	}
	log.Printf("pipeline: merge %s will retry merge-up on a later pass (%d/%d used)", taskID, attempts, maxMergeUps)
	return nil
}

// runMergeUp folds origin/<base> into the remote branch ref. Returns the
// method name on success. A *git.ConflictError return means the merge-up is
// deterministically impossible (base cannot merge into the branch cleanly);
// ErrMergeUpRace means the branch moved underneath; anything else is
// infrastructure.
func runMergeUp(ctx context.Context, cfg MergeConfig, wt string, entry *db.MergeQueueEntry, pr int, base string) (method string, err error) {
	// The tip the caller's fetch observed — GitHub rejects the update if the
	// branch has moved since (HTTP 409 → ErrMergeUpRace), so a fixer's
	// mid-flight push can never be silently built over.
	headSHA, err := git.RevParse(wt, "origin/"+entry.Branch)
	if err != nil {
		return "", fmt.Errorf("reading origin/%s: %w", entry.Branch, err)
	}
	apiErr := cfg.Gh.PRUpdateBranch(ctx, pr, headSHA)
	switch {
	case apiErr == nil:
		return "github update-branch", nil
	case errors.Is(apiErr, ErrMergeUpConflict):
		// GitHub's own three-way merge says conflict — the local fallback
		// would deterministically conflict on the same inputs. Skip it.
		return "", &git.ConflictError{}
	case errors.Is(apiErr, ErrMergeUpRace):
		return "", apiErr
	default:
		log.Printf("pipeline: merge %s: update-branch API unavailable (%v) — falling back to a local merge-up",
			entry.TaskID, apiErr)
	}
	if err := mergeUpLocal(wt, entry.Branch, base); err != nil {
		return "", err
	}
	return "local merge", nil
}

// mergeUpLocal merges origin/<base> into origin/<branch> on the REF, never
// in the checked-out task worktree: a throwaway detached worktree at the
// branch tip (the build-gate pattern), a --no-ff merge of the base, then a
// strict fast-forward push of the merge commit. A conflicted merge aborts
// and surfaces as *git.ConflictError with the file list. The worktree is
// removed on EVERY path.
func mergeUpLocal(wt, branch, base string) error {
	admin, err := git.CommonDir(wt)
	if err != nil {
		return fmt.Errorf("merge-up: repo root: %w", err)
	}
	dir, err := os.MkdirTemp("", mergeUpDirPrefix)
	if err != nil {
		return fmt.Errorf("merge-up: temp dir: %w", err)
	}
	// git worktree add wants a non-existent path; MkdirTemp reserved the
	// name, now yield it.
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("merge-up: temp dir: %w", err)
	}
	// Single cleanup point for every return below: git remove first, then a
	// raw rm + prune if git still balks (a killed merge can leave locks).
	defer func() {
		if rmErr := git.WorktreeRemove(admin, dir); rmErr != nil {
			os.RemoveAll(dir)
			_ = git.WorktreePrune(admin)
		}
	}()

	if err := git.WorktreeAddDetached(admin, dir, "origin/"+branch); err != nil {
		return fmt.Errorf("merge-up: materialize origin/%s: %w", branch, err)
	}
	if _, err := git.MergeRefs(dir, "origin/"+base,
		fmt.Sprintf("Merge %s into %s (auto merge-up)", base, branch)); err != nil {
		return err // ConflictError (already aborted) or infrastructure
	}
	// Fast-forward only: the merge commit sits on top of the exact tip we
	// materialized, so a rejection means the remote moved — a race, not
	// something to force.
	if err := git.PushHEAD(dir, "origin", branch); err != nil {
		return fmt.Errorf("merge-up: push %s: %w", branch, err)
	}
	return nil
}

// mergeUpCommentBody is the per-failed-attempt PR comment (the MAQ-16/#18
// transport): what was tried, the conflicting files, and the budget state.
func mergeUpCommentBody(branch, base string, files []string, attempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 Auto merge-up failed (attempt %d/%d): `%s` cannot be merged into `%s` cleanly.",
		attempts, maxMergeUps, base, branch)
	if len(files) > 0 {
		b.WriteString("\n\nConflicting files:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}
	if attempts < maxMergeUps {
		b.WriteString("\nThe merge gate will retry on the next pass; after 2 failed merge-ups the task parks needs-human.")
	} else {
		b.WriteString("\n\nTask parked needs-human — push a resolution, or comment `maquinista resolve` to spawn a merger session.")
	}
	return b.String()
}

// postMergeUpComment is best-effort by contract (the #18 transport): a
// failed comment post logs and never fails the conflict leg — the queue
// state remains the system of record.
func postMergeUpComment(ctx context.Context, g GhRunner, pr int, body string) {
	if g == nil {
		return
	}
	if err := g.PRPostComment(ctx, pr, body); err != nil {
		log.Printf("pipeline: PR #%d merge-up comment: %v (continuing)", pr, err)
	}
}
