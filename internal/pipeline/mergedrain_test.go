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

	// Attempt 1 (below cap): silent release — no notify churn.
	if processed, err := RunMergeDrainPass(ctx, pool, cfg, &fakeProvider{}, "team-1"); err != nil || !processed {
		t.Fatalf("attempt 1: processed=%v err=%v", processed, err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("attempt 1: entry = %q, want released to pending", got)
	}
	if texts := pipelineNotifyTextsPool(t, pool); len(texts) != 0 {
		t.Fatalf("attempt 1 notified: %q (below-cap release stays silent)", texts)
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
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || !strings.Contains(texts[0], "CI failed 2 times") {
		t.Fatalf("attempt 2 notes = %q, want exactly one CI-cap question", texts)
	}
}

// ---- integration: the TEST gate leg blocks a red branch (AC 2 + AC 5) ----

func TestRunMergeDrainPass_TestGateParksRedBranch(t *testing.T) {
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

	processed, err := RunMergeDrainPass(context.Background(), pool, cfg, &fakeProvider{}, "team-1")
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}

	// No merge — the branch is red (AC 5).
	if gh.mergeCalls != 0 {
		t.Errorf("squash-merge fired %d times on a branch with a failing test", gh.mergeCalls)
	}
	if got := entryStatus(t, pool, entry.ID); got != "failed" {
		t.Errorf("entry = %q, want failed", got)
	}
	// The task parks needs-human (AC 2)…
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Errorf("task = %s, want parked pending_approval", status)
	}
	// …with the failing STEP named in question and observation.
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("emitted %d notes, want 1: %q", len(texts), texts)
	}
	for _, want := range []string{"🆘", "test gate failed", "go test .", "parked needs-human"} {
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
		t.Errorf("task worktree %s must survive a test-gate park: %v", worktree, err)
	}
	assertNoGateLeak(t, gitRepoRoot(t, worktree))
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
