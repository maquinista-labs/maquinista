package pipeline

// Tests for the gh merge flow (EX-05). Each DB-backed test pairs a
// disposable Postgres (testPool) with a real local git remote trio — bare
// origin, admin clone, task worktree — so rebase/push/cleanup run the actual
// git machinery. GitHub is faked at the GhRunner seam.

import (
	"context"
	"time"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
)

// ---- fakes ----

type fakeGh struct {
	checks     string
	checksErr  error
	mergeCalls int
	mergeErr   error

	comments     []PRComment // returned by PRComments
	commentsErr  error
	postedBodies []string // bodies passed to PRPostComment, in order
	postErr      error
}

func (f *fakeGh) PRChecks(ctx context.Context, pr int) (string, error) {
	return f.checks, f.checksErr
}
func (f *fakeGh) PRMergeSquash(ctx context.Context, pr int) error {
	f.mergeCalls++
	return f.mergeErr
}
func (f *fakeGh) PRComments(ctx context.Context, pr int, _ time.Time) ([]PRComment, error) {
	return f.comments, f.commentsErr
}
func (f *fakeGh) PRPostComment(ctx context.Context, pr int, body string) error {
	if f.postErr != nil {
		return f.postErr
	}
	f.postedBodies = append(f.postedBodies, body)
	return nil
}

type fakeProvider struct {
	calls [][2]string // issueID → provider column ID
}

func (f *fakeProvider) IntakeIssues(ctx context.Context, teamID string) ([]Issue, error) {
	return nil, nil
}
func (f *fakeProvider) Columns(ctx context.Context, teamID string) (map[Column]string, error) {
	return map[Column]string{
		ColInProgress:      "col-ip",
		ColInReview:        "col-ir",
		ColChangesRequested: "col-cr",
		ColNeedsHuman:      "col-nh",
		ColReadyToMerge:    "col-rtm",
		ColDone:            "col-done",
	}, nil
}
func (f *fakeProvider) SetIssueColumn(ctx context.Context, issueID, columnID string) error {
	f.calls = append(f.calls, [2]string{issueID, columnID})
	return nil
}

func (f *fakeProvider) AddIssueLink(ctx context.Context, issueID, url string) error {
	return nil
}

// ---- git harness ----

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitCommitFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	os.MkdirAll(filepath.Dir(full), 0755)
	os.WriteFile(full, []byte(content), 0644)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "update "+path)
}

// initRemoteTrio creates a bare origin, an admin clone (working repo), and a
// linked task worktree on a feature branch pushed to origin. Returns the
// admin clone and task worktree paths.
func initRemoteTrio(t *testing.T, taskID string) (admin, worktree string) {
	t.Helper()
	origin := t.TempDir()
	admin = t.TempDir()
	worktree = t.TempDir()

	gitRun(t, origin, "init", "--bare", ".")
	gitRun(t, admin, "clone", origin, ".")
	gitRun(t, admin, "config", "user.email", "merger@test.com")
	gitRun(t, admin, "config", "user.name", "Merger")
	gitRun(t, admin, "checkout", "-B", "main")
	gitCommitFile(t, admin, "README.md", "# repo\n")
	gitRun(t, admin, "push", "origin", "main")

	branch := "t-" + taskID + "/feature"
	gitRun(t, admin, "worktree", "add", "-b", branch, worktree, "origin/main")
	gitRun(t, admin, "push", "origin", branch)
	return admin, worktree
}

// seedReadyTask inserts a ready_to_merge pipeline task with a PR URL, a
// worktree, and a ticket_issue_id, then enqueues + claims a merge entry.
func seedReadyTask(t *testing.T, pool *pgxpool.Pool, worktree string) *db.MergeQueueEntry {
	t.Helper()
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, pr_state, metadata)
		VALUES ($1, 'merge me', 'ready_to_merge', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/99', 'open',
		        $3::jsonb)
	`, taskID, worktree, `{"ticket_issue_id":"issue-1"}`)
	execOK(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, role, task_id, status,
		                    runner_type, cwd, window_name, started_at, last_seen, stop_requested)
		VALUES ('worker-`+taskID+`', 'sess', 'w', 'implementor', $1, 'dead',
		        'pi', $2, 'w', NOW(), NOW(), FALSE)
	`, taskID, worktree)
	branch := gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	execOK(t, pool, `
		INSERT INTO merge_queue (task_id, agent_id, branch, worktree_dir, base_branch, commit_sha)
		VALUES ($1, 'merger', $2, $3, 'main', '')
	`, taskID, branch, worktree)

	entry, err := db.ClaimMergeEntry(pool)
	if err != nil || entry == nil {
		t.Fatalf("claiming seeded merge entry: %v (entry %v)", err, entry)
	}
	return entry
}

var taskCounter int

func nextTaskNum() int { taskCounter++; return taskCounter }

func taskRow(t *testing.T, pool *pgxpool.Pool, taskID string) (status, prState string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(pr_state,'') FROM tasks WHERE id = $1`, taskID).
		Scan(&status, &prState)
	if err != nil {
		t.Fatalf("loading task %s: %v", taskID, err)
	}
	return status, prState
}

func entryStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM merge_queue WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("loading entry %d: %v", id, err)
	}
	return status
}

func observationCount(t *testing.T, pool *pgxpool.Pool, taskID, agentID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM task_context WHERE task_id = $1 AND agent_id = $2`,
		taskID, agentID).Scan(&n); err != nil {
		t.Fatalf("counting observations: %v", err)
	}
	return n
}

// ---- config ----

func TestMergeConfigFromEnv(t *testing.T) {
	// Hermetic defaults: the operator's shell may export PIPELINE_* (this
	// pane runs with MERGE_MODE=gh + AUTO_MERGE=1), so pin the ambient
	// knobs to their unset spellings before asserting defaults.
	t.Setenv("PIPELINE_MERGE_MODE", "")
	t.Setenv("PIPELINE_AUTO_MERGE", "")
	t.Setenv("MAQUINISTA_MERGE_ATTEMPTS_MAX", "")
	cfg := MergeConfigFromEnv()
	if cfg.Mode != MergeModeLocal {
		t.Errorf("default mode = %q, want local", cfg.Mode)
	}
	if cfg.AutoMerge {
		t.Error("auto-merge must default to off")
	}
	if cfg.MaxAttempts != defaultMergeAttempts {
		t.Errorf("default max attempts = %d, want %d", cfg.MaxAttempts, defaultMergeAttempts)
	}

	t.Setenv("PIPELINE_MERGE_MODE", "gh")
	t.Setenv("PIPELINE_AUTO_MERGE", "1")
	t.Setenv("MAQUINISTA_MERGE_ATTEMPTS_MAX", "7")
	cfg = MergeConfigFromEnv()
	if cfg.Mode != MergeModeGH || !cfg.AutoMerge {
		t.Errorf("got mode=%q auto=%v, want gh+true", cfg.Mode, cfg.AutoMerge)
	}
	if cfg.MaxAttempts != 7 {
		t.Errorf("max attempts = %d, want 7", cfg.MaxAttempts)
	}

	// EX-06 reviewer nit: boolean knobs accept the usual truthy spellings.
	for _, v := range []string{"yes", "True", "t", "Y"} {
		t.Setenv("PIPELINE_AUTO_MERGE", v)
		if cfg := MergeConfigFromEnv(); !cfg.AutoMerge {
			t.Errorf("AUTO_MERGE=%q not accepted", v)
		}
	}
	t.Setenv("PIPELINE_AUTO_MERGE", "")
	if cfg := MergeConfigFromEnv(); cfg.AutoMerge {
		t.Error("empty AUTO_MERGE must stay off")
	}

	// Garbage attempts cap falls back to the default instead of 0.
	t.Setenv("MAQUINISTA_MERGE_ATTEMPTS_MAX", "banana")
	if cfg := MergeConfigFromEnv(); cfg.MaxAttempts != defaultMergeAttempts {
		t.Errorf("garbage cap → %d, want default %d", cfg.MaxAttempts, defaultMergeAttempts)
	}
}

// ---- enqueue pass ----

func TestMergeEnqueuePass(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "pass1")
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, metadata)
		VALUES ($1, 'queued', 'ready_to_merge', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/98', '{"ticket_issue_id":"issue-2"}'::jsonb)
	`, taskID, worktree)

	cfg := MergeConfig{Mode: MergeModeGH}
	n, err := RunMergeEnqueuePass(context.Background(), pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("enqueued %d entries, want 1", n)
	}
	entry, err := db.GetPendingMergeEntryByTask(pool, taskID)
	if err != nil || entry == nil {
		t.Fatalf("pending entry missing: %v", err)
	}
	if entry.Branch != gitRun(t, worktree, "rev-parse", "--abbrev-ref", "HEAD") {
		t.Errorf("branch = %q, want derived worktree branch", entry.Branch)
	}

	// Second pass: no duplicate.
	n, err = RunMergeEnqueuePass(context.Background(), pool, cfg)
	if err != nil || n != 0 {
		t.Fatalf("second pass enqueued %d (err %v), want 0", n, err)
	}

	// Local mode is a no-op.
	n, _ = RunMergeEnqueuePass(context.Background(), pool, MergeConfig{Mode: MergeModeLocal})
	if n != 0 {
		t.Errorf("local mode enqueued %d entries, want 0", n)
	}
}

// ---- happy path (C4) ----

func TestProcessMergeGH_HappyPath(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "happy")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	gh := &fakeGh{checks: ChecksGreen}
	prov := &fakeProvider{}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, prov, "team-1", entry); err != nil {
		t.Fatal(err)
	}

	status, prState := taskRow(t, pool, taskID)
	if status != "done" || prState != "merged" {
		t.Errorf("task = %s/%s, want done/merged", status, prState)
	}
	if got := entryStatus(t, pool, entry.ID); got != "merged" {
		t.Errorf("entry status = %q, want merged", got)
	}
	if gh.mergeCalls != 1 {
		t.Errorf("gh merge calls = %d, want 1", gh.mergeCalls)
	}
	if len(prov.calls) != 1 || prov.calls[0] != [2]string{"issue-1", "col-done"} {
		t.Errorf("provider calls = %v, want [issue-1 col-done]", prov.calls)
	}
	if observationCount(t, pool, taskID, "merger") == 0 {
		t.Error("expected a merger observation")
	}
	// Cleanup: worktree gone, local + remote branch gone.
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Error("task worktree should be removed after merge")
	}
	if out := gitRun(t, admin, "branch", "--list", entry.Branch); out != "" {
		t.Errorf("local branch should be deleted, got %q", out)
	}
	if out := gitRun(t, admin, "ls-remote", "--heads", "origin", entry.Branch); out != "" {
		t.Errorf("remote branch should be deleted, got %q", out)
	}
}

// ---- rebase conflict (C5) ----

func TestProcessMergeGH_Conflict(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "conflict")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	// Advance main with a change that collides with the branch.
	admin := gitRepoRoot(t, worktree)
	gitRun(t, admin, "checkout", "main")
	gitCommitFile(t, admin, "feature.txt", "main wins\n")
	gitRun(t, admin, "push", "origin", "main")
	gitCommitFile(t, worktree, "feature.txt", "branch wins\n")
	gitRun(t, worktree, "push", "origin", entry.Branch)

	gh := &fakeGh{checks: ChecksGreen}
	prov := &fakeProvider{}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, prov, "team-1", entry); err != nil {
		t.Fatal(err)
	}

	status, _ := taskRow(t, pool, taskID)
	if status != "pending_approval" {
		t.Errorf("task status = %q, want pending_approval (needs human)", status)
	}
	if got := entryStatus(t, pool, entry.ID); got != "conflict" {
		t.Errorf("entry status = %q, want conflict", got)
	}
	if gh.mergeCalls != 0 || len(prov.calls) != 0 {
		t.Errorf("merge/board must not fire on conflict (gh=%d, board=%v)", gh.mergeCalls, prov.calls)
	}
	if observationCount(t, pool, taskID, "merger") == 0 {
		t.Error("expected a conflict observation")
	}
	// Worktree must be left usable (rebase aborted, clean).
	if out := gitRun(t, worktree, "status", "--porcelain"); out != "" {
		t.Errorf("worktree dirty after conflict: %q", out)
	}
}

// ---- CI gate (C6) ----

func TestProcessMergeGH_CIGate(t *testing.T) {
	for _, tc := range []struct {
		checks    string
		wantMerge bool
	}{
		{ChecksGreen, true},
		{ChecksNone, true}, // no checks configured: vacuously green
		{ChecksPending, false},
		{ChecksFailed, false},
	} {
		t.Run(tc.checks, func(t *testing.T) {
			pool := testPool(t)
			_, worktree := initRemoteTrio(t, "ci")
			entry := seedReadyTask(t, pool, worktree)
			taskID := entry.TaskID

			gh := &fakeGh{checks: tc.checks}
			cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}
			if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
				t.Fatal(err)
			}

			if tc.wantMerge {
				if gh.mergeCalls != 1 {
					t.Errorf("%s: merge calls = %d, want 1", tc.checks, gh.mergeCalls)
				}
				if status, _ := taskRow(t, pool, taskID); status != "done" {
					t.Errorf("%s: task = %s, want done", tc.checks, status)
				}
				return
			}
			if gh.mergeCalls != 0 {
				t.Errorf("%s: merge fired with %s checks", tc.checks, tc.checks)
			}
			if got := entryStatus(t, pool, entry.ID); got != "pending" {
				t.Errorf("%s: entry status = %q, want released to pending", tc.checks, got)
			}
			if status, _ := taskRow(t, pool, taskID); status != "ready_to_merge" {
				t.Errorf("%s: task = %s, want ready_to_merge", tc.checks, status)
			}
		})
	}
}

// ---- human gate (C7) ----

func TestProcessMergeGH_AutoMergeOff(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "gate")
	entry := seedReadyTask(t, pool, worktree)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: false, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if gh.mergeCalls != 0 {
		t.Error("PRMergeSquash must not fire with auto-merge off")
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Errorf("entry status = %q, want pending (waiting)", got)
	}
	if status, _ := taskRow(t, pool, entry.TaskID); status != "ready_to_merge" {
		t.Errorf("task status = %s, want ready_to_merge", status)
	}
}

// ---- approve verb arm (C8) ----

func TestRunMergeOnApprove(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "approve")
	taskID := fmt.Sprintf("t-%d", nextTaskNum())
	execOK(t, pool, `
		INSERT INTO tasks (id, title, status, worktree_path, pr_url, metadata)
		VALUES ($1, 'approved', 'ready_to_merge', $2,
		        'https://github.com/maquinista-labs/maquinista/pull/97', '{"ticket_issue_id":"issue-3"}'::jsonb)
	`, taskID, worktree)
	branch := "t-" + taskID + "/feature"
	gitRun(t, worktree, "checkout", "-B", branch) // normalize worktree branch name

	gh := &fakeGh{checks: ChecksGreen}
	// AutoMerge=false: the human approve overrides the gate.
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: false, Gh: gh}

	if err := RunMergeOnApprove(context.Background(), pool, cfg, &fakeProvider{}, "team-1", taskID); err != nil {
		t.Fatal(err)
	}
	if gh.mergeCalls != 1 {
		t.Fatalf("merge calls = %d, want 1", gh.mergeCalls)
	}
	if status, _ := taskRow(t, pool, taskID); status != "done" {
		t.Errorf("task = %s, want done", status)
	}
}

// ---- EX-06: CI retry cap, idempotent enqueue, release guard ----

// TestProcessMergeGH_CICapParksNeedsHuman: a PR that stays red past the cap
// must fail the entry, park the task needs-human, and ask once — not churn.
func TestProcessMergeGH_CICapParksNeedsHuman(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, worktree := initRemoteTrio(t, "cicap")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	gh := &fakeGh{checks: ChecksFailed}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MaxAttempts: 2, Gh: gh}

	// Attempt 1: below cap → release to pending, one gate-red one-liner
	// (MAQ-22: a red gate is a transition; bounded by the attempts cap).
	if err := ProcessMergeGH(ctx, pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("attempt 1: entry = %q, want released to pending", got)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("attempt 1 emitted %d notes, want exactly 1 gate-red note", len(texts))
	}
	for _, want := range []string{"🟥", "gate red (ci)", "attempt 1/2", "will re-check"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("gate-red note %q missing %q", texts[0], want)
		}
	}

	// Re-claim and attempt 2: at cap → failed entry + parked task + question.
	claimed, err := db.ClaimMergeEntryByID(pool, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != entry.ID {
		t.Fatalf("re-claim got %+v, want entry %d", claimed, entry.ID)
	}
	if err := ProcessMergeGH(ctx, pool, cfg, &fakeProvider{}, "team-1", claimed); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "failed" {
		t.Fatalf("attempt 2: entry = %q, want failed", got)
	}
	if status, _ := taskRow(t, pool, taskID); status != "pending_approval" {
		t.Fatalf("attempt 2: task = %s, want parked pending_approval", status)
	}
	if gh.mergeCalls != 0 {
		t.Errorf("merge fired %d times on red PR", gh.mergeCalls)
	}
	texts = pipelineNotifyTextsPool(t, pool)
	if len(texts) != 2 {
		t.Fatalf("attempt 2 emitted %d total notes, want 2 (gate-red + cap question)", len(texts))
	}
	for _, want := range []string{"🆘", "CI failed 2 times", "parked needs-human", "maquinista approve " + taskID} {
		if !strings.Contains(texts[1], want) {
			t.Errorf("note %q missing %q", texts[1], want)
		}
	}
}

// TestProcessMergeGH_MergeFailNotifies: infrastructure failure on a green PR
// fails the entry (terminal) and leaves the task re-approvable.
func TestProcessMergeGH_MergeFailNotifies(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, worktree := initRemoteTrio(t, "mergefail")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	gh := &fakeGh{checks: ChecksGreen, mergeErr: errors.New("remote hung up")}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, MaxAttempts: 5, Gh: gh}

	if err := ProcessMergeGH(ctx, pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "failed" {
		t.Fatalf("entry = %q, want failed", got)
	}
	if status, _ := taskRow(t, pool, taskID); status != "ready_to_merge" {
		t.Fatalf("task = %s, want ready_to_merge (work is done)", status)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 || !strings.Contains(texts[0], "merge failed") {
		t.Fatalf("notes = %q, want one merge-failed note", texts)
	}
}

// TestEnqueueMerge_Idempotent: a live (pending/merging) entry wins —
// re-approval while queued must not duplicate it.
func TestEnqueueMerge_Idempotent(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "idem")
	entry := seedReadyTask(t, pool, worktree)

	err := db.EnqueueMerge(pool, entry.TaskID, entry.AgentID, "other/branch", worktree, "main", *entry.CommitSHA)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM merge_queue WHERE task_id = $1 AND status IN ('pending','merging')`,
		entry.TaskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("live entries = %d, want 1", n)
	}
}

// TestReleaseMergeEntry_GuardsTerminal: a merged entry must never be
// resurrected by a late release (double-merge prevention).
func TestReleaseMergeEntry_GuardsTerminal(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "guard")
	entry := seedReadyTask(t, pool, worktree)

	if err := db.CompleteMerge(pool, entry.ID, "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseMergeEntry(pool, entry.ID); err != nil {
		t.Fatalf("release of merged entry: %v", err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "merged" {
		t.Fatalf("entry = %q, want merged (guard must hold)", got)
	}
}

func gitRepoRoot(t *testing.T, dir string) string {
	t.Helper()
	admin, err := git.CommonDir(dir)
	if err != nil {
		t.Fatalf("common dir of %s: %v", dir, err)
	}
	return admin
}
