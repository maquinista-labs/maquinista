package pipeline

// Tests for the parked-branch merge-up pass (MAQ-42): a task parked
// pending_approval with an open PR must not let its branch rot while main
// moves. Same machinery and budget as the MAQ-26 gate leg — GitHub
// update-branch first, local ref merge fallback, 2 failed attempts with a
// PR comment each, then exactly one 🆘 and quiet. Repo side is the real git
// remote trio; GitHub is faked at the GhRunner seam.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
)

// seedParkedTask inserts a pipeline task parked pending_approval with a PR
// URL and a worktree — the MAQ-34 shape: work done, PR open, needs-human
// park, no merge_queue entry ever (the gate never saw it).
func seedParkedTask(t *testing.T, pool *pgxpool.Pool, worktree string) string {
	t.Helper()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, pr_state, metadata)
		VALUES ($1, 'parked with a PR', 'pending_approval', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/77', 'open',
		        '{"ticket_issue_id":"issue-parked"}'::jsonb)
	`, taskID, worktree)
	return taskID
}

// seedGateParkedLedger gives a parked task the terminal conflict entry a
// gate-era park leaves behind, with its merge-up budget already spent.
func seedGateParkedLedger(t *testing.T, pool *pgxpool.Pool, taskID, worktree string) {
	t.Helper()
	branch := gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch,
		                         status, conflict_files, completed_at, mergeup_attempts)
		VALUES ($1, 'merger', $2, $3, 'main', 'conflict', ARRAY['feature.txt']::text[], NOW(), 2)
	`, taskID, branch, worktree)
}

// parkedLedger is the merge_queue ledger the pass keeps for a parked task
// (nil when the task has no entry at all).
func parkedLedger(t *testing.T, pool *pgxpool.Pool, taskID string) *db.MergeQueueEntry {
	t.Helper()
	entry, err := db.LatestMergeEntryForTask(pool, taskID)
	if err != nil {
		t.Fatalf("reading parked ledger for %s: %v", taskID, err)
	}
	return entry
}

// branchContainsBase re-checks, from the task worktree, that origin/base is
// an ancestor of origin/branch — i.e. the branch was actually healed on the
// remote ref.
func branchContainsBase(t *testing.T, worktree, base, branch string) bool {
	t.Helper()
	if err := git.Fetch(worktree, "origin"); err != nil {
		t.Fatalf("fetch in %s: %v", worktree, err)
	}
	ok, err := git.IsAncestor(worktree, "origin/"+base, "origin/"+branch)
	if err != nil {
		t.Fatalf("ancestry in %s: %v", worktree, err)
	}
	return ok
}

// TestParkedMergeUpPass_HealsStaleParkedBranch (AC 1 success arm + AC 4):
// a parked branch that merges cleanly with the advanced main is folded in
// on the ref — no PR comment, no ledger entry, no budget — and the
// approve-after-park path then merges WITHOUT hitting a conflict, because
// the gate's up-to-date fast path sees the healed ref.
func TestParkedMergeUpPass_HealsStaleParkedBranch(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "pheal")
	taskID := seedParkedTask(t, pool, worktree)
	branch := gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	mergeableStalenessFixture(t, admin, worktree, branch)

	gh := &fakeGh{updateBranchFn: func() { fakeGitHubMergeUp(t, admin, branch, "main") }}
	cfg := MergeConfig{Mode: MergeModeGH, Gh: gh}

	acted, err := RunParkedMergeUpPass(context.Background(), pool, cfg, gh)
	if err != nil {
		t.Fatal(err)
	}
	if acted != 1 {
		t.Fatalf("acted = %d, want 1", acted)
	}
	if !branchContainsBase(t, worktree, "main", branch) {
		t.Error("branch was not healed: origin/main is not an ancestor of origin/<branch>")
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want still parked (a heal never merges)", status)
	}
	if e := parkedLedger(t, pool, taskID); e != nil {
		t.Errorf("ledger entry %d created on success, want none (heals are not conflicts)", e.ID)
	}
	if len(gh.postedBodies) != 0 {
		t.Errorf("PR comments = %q, want none on a successful heal", gh.postedBodies)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || !strings.Contains(texts[0], "mergeable again") {
		t.Errorf("notes = %q, want exactly the 🔀 healed note", texts)
	}
	if observationCount(t, pool, taskID, "merger") == 0 {
		t.Error("expected a heal observation")
	}

	// AC 4: the approve verb after the park re-enters the gate and merges
	// clean — the up-to-date fast path, no rebase, no conflict, no
	// merge-up comment.
	if err := db.RequeueTask(pool, taskID, "tester"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	execOK(t, pool, `UPDATE tasks SET status = 'ready_to_merge' WHERE id = $1`, taskID)
	approveCfg := MergeConfig{Mode: MergeModeGH, Gh: &fakeGh{checks: ChecksGreen}}
	if err := RunMergeOnApprove(context.Background(), pool, approveCfg, &fakeProvider{}, "team-1", taskID); err != nil {
		t.Fatalf("approve after park: %v", err)
	}
	if status, prState := taskRow(t, pool, taskID); status != "done" || prState != "merged" {
		t.Errorf("task = %s/%s, want done/merged (AC 4: rebasable parked branch merges clean)", status, prState)
	}
}

// TestParkedMergeUpPass_ConflictAttemptsCapThenQuiet (AC 1, 2, 3): park a
// task with a PR, advance main into a conflict, and the pass attempts the
// merge-up — one PR comment per attempt, at the cap (2) exactly one 🆘 and
// the third tick is silent. The task never leaves pending_approval.
func TestParkedMergeUpPass_ConflictAttemptsCapThenQuiet(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "pcap")
	taskID := seedParkedTask(t, pool, worktree)
	branch := gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	conflictingFixture(t, worktree, branch)

	// The update-branch API is down → the pass takes the local fallback,
	// which really conflicts in git and names the file.
	gh := &fakeGh{updateErr: errors.New("gh: api unavailable")}
	cfg := MergeConfig{Mode: MergeModeGH, Gh: gh}
	ctx := context.Background()

	// Tick 1 (AC 3: the next tick after main moved attempts the merge-up).
	acted, err := RunParkedMergeUpPass(ctx, pool, cfg, gh)
	if err != nil || acted != 1 {
		t.Fatalf("tick 1: acted=%d err=%v", acted, err)
	}
	if n := len(gh.postedBodies); n != 1 || !strings.Contains(gh.postedBodies[0], "attempt 1/2") ||
		!strings.Contains(gh.postedBodies[0], "feature.txt") {
		t.Fatalf("tick 1: comments = %q, want one attempt-1 comment naming feature.txt", gh.postedBodies)
	}
	ledger := parkedLedger(t, pool, taskID)
	if ledger == nil || ledger.Status != "conflict" || ledger.MergeupAttempts != 1 {
		t.Fatalf("tick 1: ledger = %+v, want conflict entry with attempt 1", ledger)
	}
	if texts := pipelineNotifyTextsPool(t, pool); len(texts) != 0 {
		t.Errorf("tick 1: notes = %q, want none below the cap", texts)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("tick 1: task = %s, want still parked", status)
	}

	// Tick 2: cap reached — second comment, exactly one 🆘.
	if acted, err := RunParkedMergeUpPass(ctx, pool, cfg, gh); err != nil || acted != 1 {
		t.Fatalf("tick 2: acted=%d err=%v", acted, err)
	}
	if len(gh.postedBodies) != 2 || !strings.Contains(gh.postedBodies[1], "attempt 2/2") ||
		!strings.Contains(gh.postedBodies[1], "budget exhausted") {
		t.Fatalf("tick 2: comments = %q, want the attempt-2 cap comment", gh.postedBodies)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || !strings.Contains(texts[0], "🆘") || !strings.Contains(texts[0], "staying parked needs-human") {
		t.Fatalf("tick 2: notes = %q, want exactly one needs-human question", texts)
	}
	if ledger := parkedLedger(t, pool, taskID); ledger.MergeupAttempts != 2 {
		t.Errorf("tick 2: mergeup_attempts = %d, want 2", ledger.MergeupAttempts)
	}

	// Tick 3: quiet — no new comments, no new note, no new attempts
	// (AC 2: no repeat loop).
	if acted, err := RunParkedMergeUpPass(ctx, pool, cfg, gh); err != nil || acted != 0 {
		t.Fatalf("tick 3: acted=%d err=%v, want a silent no-op", acted, err)
	}
	if len(gh.postedBodies) != 2 {
		t.Errorf("tick 3: comments = %d, want still 2 (quiet after exhaustion)", len(gh.postedBodies))
	}
	if texts := pipelineNotifyTextsPool(t, pool); len(texts) != 1 {
		t.Errorf("tick 3: notes = %q, want still exactly the one 🆘", texts)
	}
	if ledger := parkedLedger(t, pool, taskID); ledger.MergeupAttempts != 2 {
		t.Errorf("tick 3: mergeup_attempts = %d, want still 2", ledger.MergeupAttempts)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("tick 3: task = %s, want parked (stays parked through everything)", status)
	}
}

// TestParkedMergeUpPass_GateParkedBudgetIsSpent: a task parked by the gate
// itself carries a conflict entry with mergeup_attempts at the cap — the
// pass must not re-attempt (the gate already burned both tries before
// parking) and must not re-ask.
func TestParkedMergeUpPass_GateParkedBudgetIsSpent(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "pgate")
	taskID := seedParkedTask(t, pool, worktree)
	seedGateParkedLedger(t, pool, taskID, worktree)
	conflictingFixture(t, worktree, gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD"))

	gh := &fakeGh{updateErr: errors.New("gh: api unavailable")}
	acted, err := RunParkedMergeUpPass(context.Background(), pool, MergeConfig{Mode: MergeModeGH, Gh: gh}, gh)
	if err != nil || acted != 0 {
		t.Fatalf("acted=%d err=%v, want 0 (budget already spent at the gate)", acted, err)
	}
	if len(gh.postedBodies) != 0 || gh.updateCalls != 0 {
		t.Errorf("comments=%d updateCalls=%d, want the pass untouched",
			len(gh.postedBodies), gh.updateCalls)
	}
	if len(pipelineNotifyTextsPool(t, pool)) != 0 {
		t.Error("notes fired for an exhausted park, want silence")
	}
	if ledger := parkedLedger(t, pool, taskID); ledger == nil || ledger.MergeupAttempts != 2 {
		t.Errorf("ledger = %+v, want the gate entry untouched at 2", ledger)
	}
}

// TestParkedMergeUpPass_UpToDateNoop: a parked task whose branch is current
// costs one ancestry read — no entry, no comment, no note, no attempt.
func TestParkedMergeUpPass_UpToDateNoop(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "pnoop")
	taskID := seedParkedTask(t, pool, worktree)

	gh := &fakeGh{}
	acted, err := RunParkedMergeUpPass(context.Background(), pool, MergeConfig{Mode: MergeModeGH, Gh: gh}, gh)
	if err != nil || acted != 0 {
		t.Fatalf("acted=%d err=%v, want 0", acted, err)
	}
	if gh.updateCalls != 0 || len(gh.postedBodies) != 0 {
		t.Errorf("updateCalls=%d comments=%d, want none", gh.updateCalls, len(gh.postedBodies))
	}
	if e := parkedLedger(t, pool, taskID); e != nil {
		t.Errorf("ledger entry %d created for an up-to-date branch, want none", e.ID)
	}
	if len(pipelineNotifyTextsPool(t, pool)) != 0 {
		t.Error("notes fired for an up-to-date branch, want silence")
	}
}

// TestParkedMergeUpPass_MergerEpisodeGuard: while a `resolve` merger
// session is live on a parked task, the pass must not push under it — the
// same in-flight guard the gate's conflict leg runs (MAQ-15).
func TestParkedMergeUpPass_MergerEpisodeGuard(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "pmerger")
	taskID := seedParkedTask(t, pool, worktree)
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('merger-`+taskID+`', 'sess', 'w', 'merger', $1, 'running',
		        'pi', $2, 'w', NOW(), NOW(), FALSE)
	`, taskID, worktree)
	conflictingFixture(t, worktree, gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD"))

	gh := &fakeGh{updateErr: errors.New("gh: api unavailable")}
	acted, err := RunParkedMergeUpPass(context.Background(), pool, MergeConfig{Mode: MergeModeGH, Gh: gh}, gh)
	if err != nil || acted != 0 {
		t.Fatalf("acted=%d err=%v, want 0 (merger episode in flight)", acted, err)
	}
	if gh.updateCalls != 0 || len(gh.postedBodies) != 0 {
		t.Errorf("updateCalls=%d comments=%d, want none under a live merger", gh.updateCalls, len(gh.postedBodies))
	}
}

// TestParkedMergeUpPass_InertGuards: local mode and a nil runner are
// no-ops — the pass is gh-mode machinery.
func TestParkedMergeUpPass_InertGuards(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "pinert")
	taskID := seedParkedTask(t, pool, worktree)

	if n, err := RunParkedMergeUpPass(context.Background(), pool, MergeConfig{Mode: MergeModeLocal}, &fakeGh{}); err != nil || n != 0 {
		t.Fatalf("local mode: acted=%d err=%v, want inert", n, err)
	}
	if n, err := RunParkedMergeUpPass(context.Background(), pool, MergeConfig{Mode: MergeModeGH}, nil); err != nil || n != 0 {
		t.Fatalf("nil runner: acted=%d err=%v, want inert", n, err)
	}
	if e := parkedLedger(t, pool, taskID); e != nil {
		t.Errorf("inert pass created ledger entry %d", e.ID)
	}
}
