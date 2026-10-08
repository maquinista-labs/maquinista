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
//
// Parked-branch leg (MAQ-42). The gate leg above only runs for
// ready_to_merge tasks — a task parked pending_approval holds a PR whose
// branch rots while main moves, and the eventual human approve re-runs
// straight into the conflict (the 07/10 MAQ-34 park sat CONFLICTING for a
// day because no pass ever considered the branch). RunParkedMergeUpPass is
// the dispatch-tick arm that heals those branches with the same machinery:
// a stale parked branch gets the same update-branch-then-local-merge
// merge-up; success folds main in and the task stays parked but mergeable
// (the gate's up-to-date fast path takes it when the human moves); a
// conflicting branch consumes the SAME 2-attempt budget — held on the
// task's merge_queue entry (created as a terminal 'conflict' ledger row
// when the park never reached the gate) — with a PR comment per attempt,
// and goes quiet after the second failure behind exactly one 🆘. A
// gate-parked conflict entry arrives with its budget spent, so a parked
// conflict is never re-attempted; a `resolve` merger episode in flight
// blocks the pass entirely, same as it blocks the gate.

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
	fan := parkFanout{gh: ghPoster(cfg.Gh), prov: prov} // MAQ-34: park/failure arms below

	method, upErr := runMergeUp(ctx, cfg, wt, entry, pr, base)
	if upErr == nil {
		// Re-check mergeability before re-entering the gates: the healed
		// ref must actually contain the base now. This is the guard against
		// a claimed-but-unsynced API success or a lost push — it counts as
		// a failed attempt, never as a mergeable branch.
		if err := git.Fetch(wt, "origin"); err != nil {
			return failMerge(ctx, pool, fan, entry.ID, taskID, fmt.Sprintf("merge-up verification fetch failed: %v", err))
		}
		mergeable, err := git.IsAncestor(wt, "origin/"+base, "origin/"+entry.Branch)
		if err != nil {
			return failMerge(ctx, pool, fan, entry.ID, taskID, fmt.Sprintf("merge-up verification failed: %v", err))
		}
		if mergeable {
			db.AddObservation(pool, taskID, "merger",
				fmt.Sprintf("Auto merge-up healed stale branch %s: origin/%s folded in via %s; re-checked mergeable — proceeding to gates.",
					entry.Branch, base, method))
			notifyTaskf(ctx, pool, taskID, "🔀 %s: branch %s was stale — auto merge-up (%s) folded origin/%s in; merge gates re-running.%s",
				taskTitle(ctx, pool, taskID), entry.Branch, method, base, prLinkSuffix(ctx, pool, taskID))
			log.Printf("pipeline: merge %s conflict healed by auto merge-up (%s) on branch %s", taskID, method, entry.Branch)
			return finishMergeGH(ctx, pool, cfg, fan, prov, teamID, entry, info, wt, base, pr)
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
		return failMerge(ctx, pool, fan, entry.ID, taskID, fmt.Sprintf("auto merge-up failed: %v", upErr))
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
	postMergeUpComment(ctx, cfg.Gh, pr, mergeUpCommentBody(entry.Branch, base, files, attempts, false))
	log.Printf("pipeline: merge %s auto merge-up conflicted on branch %s (attempt %d/%d): %v",
		taskID, entry.Branch, attempts, maxMergeUps, conflict)

	if attempts >= maxMergeUps {
		return parkMergeConflict(ctx, pool, fan, taskID, entry, &git.ConflictError{Files: files})
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
// alreadyParked (MAQ-42) adjusts the trailer for the parked-branch pass —
// the task is ALREADY needs-human, so the comment reports a failed heal
// instead of announcing a park.
func mergeUpCommentBody(branch, base string, files []string, attempts int, alreadyParked bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 Auto merge-up failed (attempt %d/%d): `%s` cannot be merged into `%s` cleanly.",
		attempts, maxMergeUps, base, branch)
	if len(files) > 0 {
		b.WriteString("\n\nConflicting files:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}
	if alreadyParked {
		if attempts < maxMergeUps {
			b.WriteString("\n\nThe parked-branch merge-up will retry on a later pass; after 2 failed merge-ups it goes quiet — the task keeps its needs-human 🆘.")
		} else {
			b.WriteString("\n\nMerge-up budget exhausted — the task stays parked needs-human and this pass goes quiet. Push a resolution, or comment `maquinista resolve` to spawn a merger session.")
		}
		return b.String()
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

// ---- parked-branch merge-up (MAQ-42) ------------------------------------

// parkedMergeUpCandidatesSQL is the pass's scan: pipeline tasks parked
// pending_approval that still hold an open PR and a worktree, and have no
// live merge_queue entry (the drain owns those — the status guard in
// ProcessMergeGH releases them untouched, and a park always lands its own
// entry terminal first).
const parkedMergeUpCandidatesSQL = `
SELECT t.id, t.pr_url, t.worktree_path
FROM   tasks t
WHERE  t.status = 'pending_approval'
  AND  t.metadata->>'ticket_issue_id' IS NOT NULL
  AND  t.pr_url IS NOT NULL AND t.pr_url <> ''
  AND  t.worktree_path IS NOT NULL AND t.worktree_path <> ''
  AND  NOT EXISTS (
       SELECT 1 FROM merge_queue q
       WHERE  q.task_id = t.id
         AND  q.status IN ('pending', 'merging'))`

// parkedMergeUpPass is the dispatch-loop arm of the parked-branch leg:
// gh merge mode only (the PR tooling is the GhRunner's), regardless of
// auto-merge — branch hygiene serves the approve verb exactly as it serves
// the drain. The runner comes from the dispatch wiring (cfg.Gh), like every
// other pass; the env only selects the mode.
func parkedMergeUpPass(ctx context.Context, pool *pgxpool.Pool, g GhRunner) error {
	_, err := RunParkedMergeUpPass(ctx, pool, MergeConfigFromEnv(), g)
	return err
}

// RunParkedMergeUpPass is one dispatch-tick iteration of the parked-branch
// merge-up (MAQ-42): every parked task holding a PR whose branch went stale
// against origin's default gets the same fold-main-in the gate path runs —
// GitHub's update-branch API first, a local ref merge as fallback — so the
// eventual human move (approve, requeue, resolve) never re-enters the gate
// with a DIRTY branch that was rebasable. Returns the number of parked
// tasks acted on (healed, or a failed attempt consumed). Candidates are
// processed independently: one task's git/gh trouble never blocks the
// rest.
func RunParkedMergeUpPass(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, g GhRunner) (int, error) {
	if cfg.Mode != MergeModeGH || g == nil {
		return 0, nil
	}
	rows, err := pool.Query(ctx, parkedMergeUpCandidatesSQL)
	if err != nil {
		return 0, fmt.Errorf("pipeline: parked merge-up scan: %w", err)
	}
	type cand struct{ taskID, prURL, worktree string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.taskID, &c.prURL, &c.worktree); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pipeline: parked merge-up scan: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pipeline: parked merge-up rows: %w", err)
	}

	acted := 0
	for _, c := range cands {
		did, err := parkedMergeUpOne(ctx, pool, cfg, g, c.taskID, c.prURL, c.worktree)
		if err != nil {
			log.Printf("pipeline: parked merge-up %s: %v", c.taskID, err)
			continue
		}
		if did {
			acted++
		}
	}
	return acted, nil
}

// parkedMergeUpOne runs the merge-up leg for one parked task. Every skip is
// a logged no-op — only a merge-up attempt or a heal reports acted=true.
// The budget lives on the task's merge_queue entry (EnsureParkedMergeUpLedger
// creates a terminal 'conflict' ledger row for parks that never reached the
// gate), so exhaustion survives restarts and a gate-parked conflict entry —
// which arrives with its 2/2 spent — is never re-attempted.
func parkedMergeUpOne(ctx context.Context, pool *pgxpool.Pool, cfg MergeConfig, g GhRunner, taskID, prURL, wt string) (bool, error) {
	pr, err := prFromPullURL(prURL)
	if err != nil {
		log.Printf("pipeline: parked merge-up %s: %v", taskID, err)
		return false, nil
	}
	// A `resolve` merger session owns a parked branch while its episode is
	// in flight — the same guard the gate's conflict leg runs (MAQ-15): the
	// merge-up must never push under a mid-resolution merger.
	pending, err := mergerEpisodePending(ctx, pool, taskID)
	if err != nil {
		log.Printf("pipeline: parked merge-up %s: merger episode check: %v", taskID, err)
	}
	if pending {
		return false, nil
	}
	if err := git.Fetch(wt, "origin"); err != nil {
		log.Printf("pipeline: parked merge-up %s: fetch: %v", taskID, err)
		return false, nil
	}
	base, err := defaultBranch(wt)
	if err != nil {
		base = "main"
	}
	branch, err := git.CurrentBranch(wt)
	if err != nil {
		log.Printf("pipeline: parked merge-up %s: branch: %v", taskID, err)
		return false, nil
	}
	if exists, err := git.RefExists(wt, "origin/"+branch); err != nil {
		return false, fmt.Errorf("mergeability check: %w", err)
	} else if !exists {
		log.Printf("pipeline: parked merge-up %s: origin/%s gone — skipping", taskID, branch)
		return false, nil
	}
	// The every-tick fast path: an up-to-date parked branch (or one the
	// pass already healed, or a human pushed the merge themselves) costs
	// one ancestry read and nothing else.
	upToDate, err := git.IsAncestor(wt, "origin/"+base, "origin/"+branch)
	if err != nil {
		return false, fmt.Errorf("mergeability check: %w", err)
	}
	if upToDate {
		return false, nil
	}
	// Exhausted parks go quiet BEFORE any attempt: a gate-parked conflict
	// entry arrives at 2/2, and our own exhaustion is permanent for the
	// parked episode (the human escape is `resolve` / requeue).
	latest, err := db.LatestMergeEntryForTask(pool, taskID)
	if err != nil {
		return false, fmt.Errorf("ledger check: %w", err)
	}
	if latest != nil && latest.MergeupAttempts >= maxMergeUps {
		return false, nil
	}

	probe := &db.MergeQueueEntry{TaskID: taskID, Branch: branch, WorktreeDir: wt, BaseBranch: base}
	cfg.Gh = g
	method, upErr := runMergeUp(ctx, cfg, wt, probe, pr, base)
	if upErr == nil {
		// Verify like the gate: a claimed-but-unsynced API success counts
		// as a failed attempt, never as a healed branch.
		if err := git.Fetch(wt, "origin"); err != nil {
			return false, fmt.Errorf("verification fetch: %w", err)
		}
		mergeable, err := git.IsAncestor(wt, "origin/"+base, "origin/"+branch)
		if err != nil {
			return false, fmt.Errorf("verification: %w", err)
		}
		if mergeable {
			db.AddObservation(pool, taskID, "merger",
				fmt.Sprintf("Parked branch %s was stale — auto merge-up (%s) folded origin/%s in; mergeable again for the next approve.",
					branch, method, base))
			notifyTaskf(ctx, pool, taskID, "🔀 %s: parked branch %s was stale — auto merge-up (%s) folded origin/%s in; still parked, but the branch is mergeable again.%s",
				taskTitle(ctx, pool, taskID), branch, method, base, prLinkSuffix(ctx, pool, taskID))
			log.Printf("pipeline: parked merge-up healed %s branch %s via %s", taskID, branch, method)
			return true, nil
		}
		log.Printf("pipeline: parked merge-up %s claimed success but origin/%s still lacks origin/%s — counting as a failed attempt",
			taskID, branch, base)
		upErr = &git.ConflictError{}
	}
	// A race (branch moved underneath the merge-up — a fixer/merger push)
	// changed the state the staleness was diagnosed in: retry next tick,
	// no budget consumed (gate semantics).
	if errors.Is(upErr, ErrMergeUpRace) {
		log.Printf("pipeline: parked merge-up %s raced on branch %s — retrying next tick", taskID, branch)
		return false, nil
	}
	// Any other non-conflict failure is infrastructure (network, gh, push
	// transport): transient, so no budget — the next tick retries.
	var conflict *git.ConflictError
	if !errors.As(upErr, &conflict) {
		log.Printf("pipeline: parked merge-up %s failed (infra, will retry): %v", taskID, upErr)
		return false, nil
	}

	// Deterministic conflict: ensure the ledger, consume one attempt,
	// comment on the PR. The API's 422 carries no file list — fall back to
	// what the ledger already holds (a gate-era entry named the original
	// overlap there).
	entry, err := db.EnsureParkedMergeUpLedger(pool, taskID, branch, wt, base)
	if err != nil {
		return false, fmt.Errorf("ledger: %w", err)
	}
	if entry == nil {
		log.Printf("pipeline: parked merge-up %s: live merge entry appeared — skipping", taskID)
		return false, nil
	}
	files := conflict.Files
	if len(files) == 0 {
		files = entry.ConflictFiles
	}
	attempts, err := db.BumpParkedMergeUpAttempts(pool, entry.ID, files)
	if err != nil {
		return false, fmt.Errorf("bumping merge-up attempts: %w", err)
	}
	postMergeUpComment(ctx, g, pr, mergeUpCommentBody(branch, base, files, attempts, true))
	log.Printf("pipeline: parked merge-up %s conflicted on branch %s (attempt %d/%d): %v",
		taskID, branch, attempts, maxMergeUps, upErr)
	if attempts >= maxMergeUps {
		// Exactly once, by construction: the cap branch fires on the bump
		// that REACHES it, and the exhausted pre-check keeps every later
		// tick away from this code path.
		db.AddObservation(pool, taskID, "merger",
			fmt.Sprintf("Parked branch %s still conflicts with origin/%s after %d merge-up attempts — staying parked, pass goes quiet.", branch, base, attempts))
		notifyTaskf(ctx, pool, taskID, "🆘 %s: parked branch %s still conflicts with origin/%s after %d merge-up attempts — staying parked needs-human. Push a resolution, or comment `maquinista resolve` on the PR to spawn a merger session.%s",
			taskTitle(ctx, pool, taskID), branch, base, attempts, prLinkSuffix(ctx, pool, taskID))
	}
	return true, nil
}
