package pipeline

// Tests for the daemon-native merge executor (MAQ-21): RunMergeDrainPass
// claims the oldest pending merge_queue entry and drives it through the
// same guarded ProcessMergeGH arms the approve verb uses — the external
// bash watcher it replaces could not reuse those internals. DB-backed tests
// pair a disposable Postgres (testPool) with the real git remote trio;
// GitHub is faked at the GhRunner seam.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// seedReadyTaskPending is seedReadyTask without the claim, with a
// controllable queue age (drain order = enqueued_at ASC).
func seedReadyTaskPending(t *testing.T, pool *pgxpool.Pool, worktree string, age time.Duration) *db.MergeQueueEntry {
	t.Helper()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, pr_state, metadata)
		VALUES ($1, 'drain me', 'ready_to_merge', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/98', 'open',
		        '{"ticket_issue_id":"issue-drain"}'::jsonb)
	`, taskID, worktree)
	branch := gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, commit_sha, enqueued_at)
		VALUES ($1, 'merger', $2, $3, 'main', '', NOW() - make_interval(secs => $4))
	`, taskID, branch, worktree, age.Seconds())
	entry, err := db.GetPendingMergeEntryByTask(pool, taskID)
	if err != nil || entry == nil {
		t.Fatalf("seeding pending entry for %s: %v (entry %v)", taskID, err, entry)
	}
	return entry
}

// ---- inert guards ----

func TestRunMergeDrainPass_InertGuards(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "inert")
	entry := seedReadyTaskPending(t, pool, worktree, 0)

	gh := &fakeGh{checks: ChecksGreen}

	// Local mode: the legacy engine owns merges.
	processed, err := RunMergeDrainPass(context.Background(), pool,
		MergeConfig{Mode: MergeModeLocal, AutoMerge: true, Gh: gh}, &fakeProvider{}, "team-1")
	if err != nil || processed {
		t.Fatalf("local mode must be inert: processed=%v err=%v", processed, err)
	}

	// Auto-merge off: those merges belong to the approve verb.
	processed, err = RunMergeDrainPass(context.Background(), pool,
		MergeConfig{Mode: MergeModeGH, AutoMerge: false, Gh: gh}, &fakeProvider{}, "team-1")
	if err != nil || processed {
		t.Fatalf("auto-merge off must be inert: processed=%v err=%v", processed, err)
	}

	// gh + auto-merge but no runner: a wiring error, surfaced as such.
	if _, err := RunMergeDrainPass(context.Background(), pool,
		MergeConfig{Mode: MergeModeGH, AutoMerge: true}, &fakeProvider{}, "team-1"); err == nil {
		t.Fatal("missing GhRunner must error, not silently no-op")
	}

	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Errorf("inert passes must not touch the entry, got %q", got)
	}
	if gh.mergeCalls != 0 {
		t.Errorf("inert passes merged %d times", gh.mergeCalls)
	}
}

// ---- drain order (AC 1: entries drain daemon-side, oldest first) ----

func TestRunMergeDrainPass_OldestFirst(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, wt1 := initRemoteTrio(t, "drain1")
	first := seedReadyTaskPending(t, pool, wt1, 2*time.Minute) // older
	_, wt2 := initRemoteTrio(t, "drain2")
	second := seedReadyTaskPending(t, pool, wt2, 0) // newer

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	// Pass 1: only the oldest entry is claimed and merged.
	processed, err := RunMergeDrainPass(ctx, pool, cfg, &fakeProvider{}, "team-1")
	if err != nil || !processed {
		t.Fatalf("pass 1: processed=%v err=%v", processed, err)
	}
	if got := entryStatus(t, pool, first.ID); got != "merged" {
		t.Errorf("oldest entry = %q, want merged", got)
	}
	if status, prState := taskRow(t, pool, first.TaskID); status != "done" || prState != "merged" {
		t.Errorf("first task = %s/%s, want done/merged", status, prState)
	}
	if got := entryStatus(t, pool, second.ID); got != "pending" {
		t.Errorf("newer entry must wait its turn, got %q", got)
	}

	// Pass 2: the queue drains FIFO.
	if processed, err := RunMergeDrainPass(ctx, pool, cfg, &fakeProvider{}, "team-1"); err != nil || !processed {
		t.Fatalf("pass 2: processed=%v err=%v", processed, err)
	}
	if got := entryStatus(t, pool, second.ID); got != "merged" {
		t.Errorf("second entry = %q, want merged", got)
	}

	// Pass 3: empty queue.
	if processed, err := RunMergeDrainPass(ctx, pool, cfg, &fakeProvider{}, "team-1"); err != nil || processed {
		t.Fatalf("pass 3 on empty queue: processed=%v err=%v", processed, err)
	}
}

// ---- infra trouble releases the claim (no wedged 'merging' entries) ----

func TestRunMergeDrainPass_ReleasesEntryOnInfraError(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "infraerr")
	entry := seedReadyTaskPending(t, pool, worktree, 0)

	// The task row vanishes under the claim (operator script, bad migration
	// cleanup): ProcessMergeGH can record no terminal arm — the pass must
	// surface the error AND return the entry to 'pending' so a later pass
	// retries it. The queue FK enforces via RI triggers on the REFERENCED
	// table (tasks), so the simulation disables triggers there (superuser
	// testcontainer) — exactly how the row would vanish in production.
	execOK(t, pool, `ALTER TABLE tasks DISABLE TRIGGER ALL`)
	execOK(t, pool, `DELETE FROM tasks WHERE id = $1`, entry.TaskID)
	execOK(t, pool, `ALTER TABLE tasks ENABLE TRIGGER ALL`)

	gh := &fakeGh{checks: ChecksGreen}
	processed, err := RunMergeDrainPass(context.Background(), pool,
		MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}, &fakeProvider{}, "team-1")
	if !processed || err == nil {
		t.Fatalf("processed=%v err=%v, want processed=true with an error", processed, err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Errorf("entry = %q after infra error, want released to pending", got)
	}
	if gh.mergeCalls != 0 {
		t.Errorf("merge fired %d times on infra trouble", gh.mergeCalls)
	}
}

// ---- attempts cap honored through the drain (AC 3) ----

func TestRunMergeDrainPass_AttemptsCapParksNeedsHuman(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, worktree := initRemoteTrio(t, "draincap")
	entry := seedReadyTaskPending(t, pool, worktree, 0)
	taskID := entry.TaskID

	gh := &fakeGh{checks: ChecksFailed}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MaxAttempts: 2, Gh: gh}

	// Attempt 1 (below cap): released with the MAQ-22 gate-red one-liner —
	// one per distinct red, bounded by the cap.
	if processed, err := RunMergeDrainPass(ctx, pool, cfg, &fakeProvider{}, "team-1"); err != nil || !processed {
		t.Fatalf("attempt 1: processed=%v err=%v", processed, err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("attempt 1: entry = %q, want released to pending", got)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || !strings.Contains(texts[0], "gate red (ci)") || !strings.Contains(texts[0], "attempt 1/2") {
		t.Fatalf("attempt 1 notified: %q, want exactly the gate-red one-liner", texts)
	}

	// Attempt 2 (at cap): entry failed, task parked, exactly one question.
	if processed, err := RunMergeDrainPass(ctx, pool, cfg, &fakeProvider{}, "team-1"); err != nil || !processed {
		t.Fatalf("attempt 2: processed=%v err=%v", processed, err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "failed" {
		t.Fatalf("attempt 2: entry = %q, want failed", got)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Fatalf("attempt 2: task = %s, want parked pending_approval", status)
	}
	if gh.mergeCalls != 0 {
		t.Errorf("merge fired %d times on a red PR", gh.mergeCalls)
	}
	texts = pipelineNotifyTextsPool(t, pool)
	if len(texts) != 2 || !strings.Contains(texts[1], "CI failed 2 times") {
		t.Fatalf("attempt 2 notes = %q, want gate-red + exactly one CI-cap question", texts)
	}
}

// ---- startup reconcile: a dead executor's stale 'merging' claims are recovered ----

// TestReconcileStaleMerging: at drain startup, entries stuck in 'merging'
// past the threshold (daemon killed mid-pass — claims are not leased) go
// back to 'pending' with the claim timestamp cleared, while an in-flight
// claim (fresh started_at) and terminal entries are untouched.
func TestReconcileStaleMerging(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "stalemerge")
	stale := seedReadyTaskPending(t, pool, worktree, 2*time.Minute)
	_, wt2 := initRemoteTrio(t, "stalemerge2")
	inflight := seedReadyTaskPending(t, pool, wt2, 0)

	// Claim both (drain order: stale first, then inflight).
	claimed, err := db.ClaimMergeEntry(pool)
	if err != nil || claimed == nil || claimed.ID != stale.ID {
		t.Fatalf("claim 1: got %v err %v, want entry %d", claimed, err, stale.ID)
	}
	claimed2, err := db.ClaimMergeEntry(pool)
	if err != nil || claimed2 == nil || claimed2.ID != inflight.ID {
		t.Fatalf("claim 2: got %v err %v, want entry %d", claimed2, err, inflight.ID)
	}
	// Simulate the crash: the stale claim's started_at predates the
	// threshold; the in-flight one stays fresh.
	execOK(t, pool, `UPDATE merge_queue SET started_at = NOW() - interval '2 hours' WHERE id = $1`, stale.ID)
	// A terminal entry must never be resurrected by the reconcile.
	execOK(t, pool, `UPDATE merge_queue SET status = 'failed', completed_at = NOW() WHERE id = $1`, inflight.ID)

	reconcileStaleMerging(pool)

	if got := entryStatus(t, pool, stale.ID); got != "pending" {
		t.Errorf("stale claim = %q, want released to pending", got)
	}
	var startedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT started_at FROM merge_queue WHERE id = $1`, stale.ID).Scan(&startedAt); err != nil {
		t.Fatal(err)
	}
	if startedAt != nil {
		t.Errorf("released claim keeps started_at=%v — a later pass could not re-claim cleanly", startedAt)
	}
	if got := entryStatus(t, pool, inflight.ID); got != "failed" {
		t.Errorf("terminal entry = %q, want untouched failed", got)
	}

	// And the recovered entry is drainable again: the next pass claims and
	// processes it end to end (no 'vanished after enqueue' dead-end).
	gh := &fakeGh{checks: ChecksGreen}
	processed, err := RunMergeDrainPass(context.Background(), pool,
		MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}, &fakeProvider{}, "team-1")
	if err != nil || !processed {
		t.Fatalf("pass after reconcile: processed=%v err=%v", processed, err)
	}
	if got := entryStatus(t, pool, stale.ID); got != "merged" {
		t.Errorf("recovered entry = %q, want merged by the next pass", got)
	}
}

// TestReleaseStaleMergeEntries_ThresholdHonored: a claim inside the
// threshold (an in-flight pass) must not be released — the reconcile
// steals no live work.
func TestReleaseStaleMergeEntries_ThresholdHonored(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "stalethresh")
	entry := seedReadyTaskPending(t, pool, worktree, 0)
	if _, err := db.ClaimMergeEntry(pool); err != nil {
		t.Fatal(err)
	}

	ids, err := db.ReleaseStaleMergeEntries(pool, mergeStaleClaimThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("released %v — a fresh in-flight claim must be untouched", ids)
	}
	if got := entryStatus(t, pool, entry.ID); got != "merging" {
		t.Errorf("entry = %q, want still merging", got)
	}
}

// ---- integration: the TEST gate leg blocks a red branch (AC 2 + AC 5) ----

func TestRunMergeDrainPass_TestGateLoopsFixerThenParks(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "testgate")
	entry := seedReadyTaskPending(t, pool, worktree, 0)
	taskID := entry.TaskID

	// main gets a compiling module; the branch adds a failing test — the
	// exact shape `go build ./...` cannot see (build does not compile
	// _test.go files).
	pushGoBase(t, gitRepoRoot(t, worktree))
	gitCommitFile(t, worktree, "red_test.go", gateRedTest)
	gitRun(t, worktree, "push", "origin", entry.Branch)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	// Round 1: red branch → entry failed, task routed BACK to the fixer
	// (changes_requested) — the builder → review → merger loop, no human.
	processed, err := RunMergeDrainPass(context.Background(), pool, cfg, &fakeProvider{}, "team-1")
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	if gh.mergeCalls != 0 {
		t.Errorf("squash-merge fired %d times on a branch with a failing test", gh.mergeCalls)
	}
	if got := entryStatus(t, pool, entry.ID); got != "failed" {
		t.Errorf("entry = %q, want failed", got)
	}
	if status, _ := taskRow(t, pool, taskID); status != "changes_requested" {
		t.Errorf("task = %s, want changes_requested (fixer round 1)", status)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("emitted %d notes, want 1: %q", len(texts), texts)
	}
	for _, want := range []string{"🆘", "test gate failed", "go test .", "Back to the fixer", "round 1 of 3"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("note %q missing %q", texts[0], want)
		}
	}
	obs := taskContextTexts(t, pool, taskID, "merger")
	found := false
	for _, o := range obs {
		if strings.Contains(o, "Test gate failed") && strings.Contains(o, "--- FAIL: TestGateMustNotPass") {
			found = true
		}
	}
	if !found {
		t.Errorf("no test-gate observation naming the failing test, got %q", obs)
	}

	// The rejected work survives for the fix: task worktree + remote branch.
	if out := gitRun(t, gitRepoRoot(t, worktree), "ls-remote", "--heads", "origin", entry.Branch); out == "" {
		t.Error("branch vanished from the remote — the gate must not clean up work it rejected")
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("task worktree %s must survive a test-gate rejection: %v", worktree, err)
	}
	assertNoGateLeak(t, gitRepoRoot(t, worktree))

	// Rounds 2..maxGateFixRounds: the state machine keeps looping — each
	// round simulates the fixer having pushed (task re-routed to
	// ready_to_merge) and the queue re-enqueueing the branch — until the
	// round cap burns, when the task finally parks needs-human instead of
	// looping forever.
	for round := 2; round <= maxGateFixRounds; round++ {
		if _, err := pool.Exec(context.Background(),
			`UPDATE tasks SET status = 'ready_to_merge' WHERE id = $1`, taskID); err != nil {
			t.Fatal(err)
		}
		if err := db.EnqueueMerge(pool, entry.TaskID, entry.AgentID, entry.Branch, worktree, "main", *entry.CommitSHA); err != nil {
			t.Fatal(err)
		}
		if processed, err := RunMergeDrainPass(context.Background(), pool, cfg, &fakeProvider{}, "team-1"); err != nil || !processed {
			t.Fatalf("round %d: processed=%v err=%v", round, processed, err)
		}
		wantStatus, wantText := "changes_requested", "Back to the fixer"
		if round >= maxGateFixRounds {
			wantStatus, wantText = "pending_approval", "parked needs-human"
		}
		if status, _ := taskRow(t, pool, taskID); status != wantStatus {
			t.Errorf("round %d: task = %s, want %s", round, status, wantStatus)
		}
		texts = pipelineNotifyTextsPool(t, pool)
		if !strings.Contains(texts[len(texts)-1], wantText) {
			t.Errorf("round %d: note %q missing %q", round, texts[len(texts)-1], wantText)
		}
	}
}

// TestRunMergeDrainPass_TestGatePassesGreenBranch: a branch with passing
// tests merges — the gate adds latency, never a dead end.
func TestRunMergeDrainPass_TestGatePassesGreenBranch(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "testgateok")
	entry := seedReadyTaskPending(t, pool, worktree, 0)

	pushGoBase(t, gitRepoRoot(t, worktree))
	gitCommitFile(t, worktree, "ok_test.go", gatePassingTest)
	gitRun(t, worktree, "push", "origin", entry.Branch)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	processed, err := RunMergeDrainPass(context.Background(), pool, cfg, &fakeProvider{}, "team-1")
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	if gh.mergeCalls != 1 {
		t.Fatalf("merge calls = %d, want 1 (green branch must pass the gate)", gh.mergeCalls)
	}
	if status, prState := taskRow(t, pool, entry.TaskID); status != "done" || prState != "merged" {
		t.Errorf("task = %s/%s, want done/merged", status, prState)
	}
	assertNoGateLeak(t, admin)
}
