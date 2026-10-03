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

	updateCalls     int
	updateErr       error
	lastExpectedSHA string
	// updateBranchFn, when updateErr is nil, stands in for GitHub performing
	// the merge-and-push server-side (the fake cannot move git refs).
	updateBranchFn func()

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

func (f *fakeGh) PRUpdateBranch(ctx context.Context, pr int, expectedHeadSHA string) error {
	f.updateCalls++
	f.lastExpectedSHA = expectedHeadSHA
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.updateBranchFn != nil {
		f.updateBranchFn()
	}
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

// ---- MAQ-26: auto merge-up on rebase conflict ----

// conflictingFixture advances main and the branch onto the same lines of
// feature.txt — the shape that conflicts on rebase AND on merge (a
// semantic overlap the merge-up must not paper over).
func conflictingFixture(t *testing.T, worktree, branch string) {
	t.Helper()
	admin := gitRepoRoot(t, worktree)
	gitRun(t, admin, "checkout", "main")
	gitCommitFile(t, admin, "feature.txt", "main wins\n")
	gitRun(t, admin, "push", "origin", "main")
	gitCommitFile(t, worktree, "feature.txt", "branch wins\n")
	gitRun(t, worktree, "push", "origin", branch)
}

// mergeableStalenessFixture builds the MAQ-26 healing shape: rebase
// conflicts, but the branch's FINAL tree merges cleanly with main. Merge
// base carries feature.txt v0 (the branch fast-forwards onto it before its
// own commits); main rewrites line 1; the branch rewrites line 1 (C1 —
// collides with main's edit → rebase conflict) then restores it to v0 and
// appends a line (C2 — the branch's final tree no longer touches line 1 →
// a merge of main is clean).
func mergeableStalenessFixture(t *testing.T, admin, worktree, branch string) {
	t.Helper()
	gitRun(t, admin, "checkout", "main")
	gitCommitFile(t, admin, "feature.txt", "line1\nline2\n") // merge-base v0
	gitRun(t, admin, "push", "origin", "main")
	gitRun(t, worktree, "fetch", "origin")
	gitRun(t, worktree, "rebase", "origin/main") // ff the branch onto v0
	gitCommitFile(t, worktree, "feature.txt", "branch-line1\nline2\n") // C1: rebase-conflicts
	gitCommitFile(t, worktree, "feature.txt", "line1\nline2\nextra\n") // C2: merge-clean final tree
	gitRun(t, worktree, "push", "origin", branch)
	gitRun(t, admin, "fetch", "origin")
	gitCommitFile(t, admin, "feature.txt", "main-line1\nline2\n")
	gitRun(t, admin, "push", "origin", "main")
}

// fakeGitHubMergeUp performs the side effect GitHub's update-branch API has
// on success: merge origin/base into origin/branch and push the merge
// commit fast-forward — server-side, on the refs only.
func fakeGitHubMergeUp(t *testing.T, admin, branch, base string) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "up")
	gitRun(t, admin, "fetch", "origin")
	gitRun(t, admin, "worktree", "add", "--detach", wt, "origin/"+branch)
	defer func() {
		gitRun(t, admin, "worktree", "remove", "--force", wt)
		gitRun(t, admin, "worktree", "prune")
	}()
	gitRun(t, wt, "merge", "--no-ff", "-m",
		fmt.Sprintf("Merge %s into %s (github update-branch)", base, branch), "origin/"+base)
	gitRun(t, wt, "push", "origin", "HEAD:refs/heads/"+branch)
}

// assertNoMergeUpLeak is the merge-up mirror of assertNoGateLeak: no
// throwaway merge-up worktree survives on disk or in the repo registry.
func assertNoMergeUpLeak(t *testing.T, admin string) {
	t.Helper()
	if leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), mergeUpDirPrefix+"*")); len(leftovers) != 0 {
		t.Errorf("merge-up worktree(s) leaked under %s: %v", os.TempDir(), leftovers)
	}
	if out := gitRun(t, admin, "worktree", "list", "--porcelain"); strings.Contains(out, mergeUpDirPrefix) {
		t.Errorf("merge-up worktree still registered in the repo:\n%s", out)
	}
}

func mergerObservations(t *testing.T, pool *pgxpool.Pool, taskID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT content FROM task_context WHERE task_id = $1 AND agent_id = 'merger' ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("reading merger observations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning observation: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func mergeUpAttempts(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT mergeup_attempts FROM merge_queue WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("reading mergeup_attempts for %d: %v", id, err)
	}
	return n
}

// TestProcessMergeGH_Conflict (MAQ-26 rework of C5): a semantic overlap —
// the same lines changed on both sides — fails BOTH merge-up mechanisms
// deterministically. Below the cap: PR comment + released entry, task stays
// ready_to_merge. At the cap (N=2): parked needs-human exactly as before.
func TestProcessMergeGH_Conflict(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "conflict")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	admin := gitRepoRoot(t, worktree)
	conflictingFixture(t, worktree, entry.Branch)

	// GitHub 422s the update-branch (base cannot merge cleanly) — the fake
	// models the deterministic answer; the local fallback then really
	// conflicts in git.
	gh := &fakeGh{checks: ChecksGreen, updateErr: ErrMergeUpConflict}
	prov := &fakeProvider{}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	// Attempt 1: conflict → comment on the PR, entry released, task NOT parked.
	if err := ProcessMergeGH(context.Background(), pool, cfg, prov, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("attempt 1: entry = %q, want released to pending", got)
	}
	if status, _ := taskRow(t, pool, taskID); status != "ready_to_merge" {
		t.Fatalf("attempt 1: task = %s, want ready_to_merge (below cap)", status)
	}
	if n := mergeUpAttempts(t, pool, entry.ID); n != 1 {
		t.Fatalf("attempt 1: mergeup_attempts = %d, want 1", n)
	}
	if len(gh.postedBodies) != 1 || !strings.Contains(gh.postedBodies[0], "attempt 1/2") ||
		!strings.Contains(gh.postedBodies[0], "feature.txt") {
		t.Fatalf("attempt 1: PR comments = %q, want one attempt-1 comment naming feature.txt", gh.postedBodies)
	}
	// Worktree must be left usable (rebase aborted, merge-up never touched it).
	if out := gitRun(t, worktree, "status", "--porcelain"); out != "" {
		t.Errorf("worktree dirty after conflict: %q", out)
	}
	assertNoMergeUpLeak(t, admin)

	// Attempt 2: same overlap → cap reached → parked needs-human, entry conflict.
	claimed, err := db.ClaimMergeEntryByID(pool, entry.ID)
	if err != nil || claimed == nil {
		t.Fatalf("re-claim: %v (%v)", err, claimed)
	}
	if err := ProcessMergeGH(context.Background(), pool, cfg, prov, "team-1", claimed); err != nil {
		t.Fatal(err)
	}
	status, _ := taskRow(t, pool, taskID)
	if status != "pending_approval" {
		t.Errorf("attempt 2: task status = %q, want pending_approval (needs human)", status)
	}
	if got := entryStatus(t, pool, entry.ID); got != "conflict" {
		t.Errorf("attempt 2: entry status = %q, want conflict", got)
	}
	if gh.mergeCalls != 0 || len(prov.calls) != 0 {
		t.Errorf("merge/board must not fire on conflict (gh=%d, board=%v)", gh.mergeCalls, prov.calls)
	}
	if len(gh.postedBodies) != 2 || !strings.Contains(gh.postedBodies[1], "attempt 2/2") {
		t.Errorf("attempt 2: PR comments = %q, want one attempt-2 comment", gh.postedBodies)
	}
	if observationCount(t, pool, taskID, "merger") == 0 {
		t.Error("expected a conflict observation")
	}
	if out := gitRun(t, worktree, "status", "--porcelain"); out != "" {
		t.Errorf("worktree dirty after park: %q", out)
	}
}

// TestProcessMergeGH_MergeUpHealsStaleBranch (MAQ-26 acceptance): rebase
// conflicts but the branch's final tree merges cleanly — the GitHub
// update-branch API folds main in on the ref, mergeability is re-checked,
// and the gates + squash proceed. No human, no park.
func TestProcessMergeGH_MergeUpHealsStaleBranch(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "heal")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	admin = gitRepoRoot(t, worktree)
	mergeableStalenessFixture(t, admin, worktree, entry.Branch)

	gh := &fakeGh{checks: ChecksGreen, updateBranchFn: func() {
		fakeGitHubMergeUp(t, admin, entry.Branch, "main")
	}}
	prov := &fakeProvider{}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, prov, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	status, prState := taskRow(t, pool, taskID)
	if status != "done" || prState != "merged" {
		t.Fatalf("task = %s/%s, want done/merged (merge-up must heal)", status, prState)
	}
	if gh.updateCalls != 1 {
		t.Errorf("update-branch calls = %d, want 1", gh.updateCalls)
	}
	if gh.lastExpectedSHA == "" {
		t.Error("update-branch must carry the observed head SHA")
	}
	if len(gh.postedBodies) != 0 {
		t.Errorf("a healed merge-up must not comment, got %q", gh.postedBodies)
	}
	var found bool
	for _, o := range mergerObservations(t, pool, taskID) {
		if strings.Contains(o, "github update-branch") {
			found = true
		}
	}
	if !found {
		t.Error("expected a merge-up observation naming the method")
	}
	assertNoMergeUpLeak(t, admin)
}

// TestProcessMergeGH_MergeUpLocalFallback: the update-branch API is down
// (infrastructure error) → the local ref-merge fallback heals the staleness
// with a real merge commit, pushed fast-forward from a throwaway worktree —
// the task worktree stays untouched.
func TestProcessMergeGH_MergeUpLocalFallback(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "fallback")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	admin = gitRepoRoot(t, worktree)
	mergeableStalenessFixture(t, admin, worktree, entry.Branch)
	staleTip := gitRun(t, worktree, "rev-parse", "HEAD")

	gh := &fakeGh{checks: ChecksGreen, updateErr: errors.New("gh api hung up")}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "done" {
		t.Fatalf("task = %s, want done (local fallback must heal)", status)
	}
	var found bool
	for _, o := range mergerObservations(t, pool, taskID) {
		if strings.Contains(o, "local merge") {
			found = true
		}
	}
	if !found {
		t.Error("expected a merge-up observation naming the local method")
	}
	assertNoMergeUpLeak(t, admin)
	_ = staleTip // (worktree removed post-merge; tip checked in the CI-pending test)
}

// TestProcessMergeGH_MergeUpUnverifiedCountsAsFailure: an update-branch 200
// whose side effect did not land (stub without one) must NOT proceed — the
// mergeability re-check counts the attempt, comments, releases.
func TestProcessMergeGH_MergeUpUnverifiedCountsAsFailure(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "unverified")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	conflictingFixture(t, worktree, entry.Branch)

	gh := &fakeGh{checks: ChecksGreen} // updateErr nil → "success", but no side effect
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("entry = %q, want released to pending", got)
	}
	if n := mergeUpAttempts(t, pool, entry.ID); n != 1 {
		t.Fatalf("mergeup_attempts = %d, want 1 (unverified success is a failure)", n)
	}
	if len(gh.postedBodies) != 1 {
		t.Fatalf("PR comments = %q, want one", gh.postedBodies)
	}
	if status, _ := taskRow(t, pool, taskID); status != "ready_to_merge" {
		t.Errorf("task = %s, want ready_to_merge", status)
	}
}

// TestProcessMergeGH_MergeUpThenCI: the healed branch waits out its CI on a
// released entry; the next pass takes the up-to-date fast path (no rebase,
// no re-push) straight through the gates. Also proves the worktree-discipline:
// the checked-out task worktree keeps its stale tip while the ref moves.
func TestProcessMergeGH_MergeUpThenCI(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "muci")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	admin = gitRepoRoot(t, worktree)
	mergeableStalenessFixture(t, admin, worktree, entry.Branch)
	staleTip := gitRun(t, worktree, "rev-parse", "HEAD")

	gh := &fakeGh{checks: ChecksPending, updateBranchFn: func() {
		fakeGitHubMergeUp(t, admin, entry.Branch, "main")
	}}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	// Pass 1: healed, CI pending → released.
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("pass 1: entry = %q, want released (CI pending)", got)
	}
	// Ref moved, worktree did not — the fixer in the pane never notices.
	if got := gitRun(t, worktree, "rev-parse", "HEAD"); got != staleTip {
		t.Errorf("worktree HEAD moved to %s, want stale %s", got, staleTip)
	}
	if out := gitRun(t, worktree, "status", "--porcelain"); out != "" {
		t.Errorf("worktree dirty after merge-up: %q", out)
	}

	// Pass 2: CI green → fast path → merged.
	gh.checks = ChecksGreen
	claimed, err := db.ClaimMergeEntryByID(pool, entry.ID)
	if err != nil || claimed == nil {
		t.Fatalf("re-claim: %v (%v)", err, claimed)
	}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", claimed); err != nil {
		t.Fatal(err)
	}
	status, _ := taskRow(t, pool, taskID)
	if status != "done" {
		t.Fatalf("pass 2: task = %s, want done", status)
	}
	if gh.updateCalls != 1 {
		t.Errorf("update-branch calls = %d, want 1 (fast path must not re-merge)", gh.updateCalls)
	}
}

// TestProcessMergeGH_MergeUpRace: the branch moved under the merge-up
// (HTTP 409) — state changed, so the entry releases WITHOUT consuming
// budget; the next pass re-fetches and re-decides.
func TestProcessMergeGH_MergeUpRace(t *testing.T) {
	pool := testPool(t)
	_, worktree := initRemoteTrio(t, "race")
	entry := seedReadyTask(t, pool, worktree)
	conflictingFixture(t, worktree, entry.Branch)

	gh := &fakeGh{checks: ChecksGreen, updateErr: ErrMergeUpRace}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("entry = %q, want released to pending", got)
	}
	if n := mergeUpAttempts(t, pool, entry.ID); n != 0 {
		t.Errorf("mergeup_attempts = %d, want 0 (races are free)", n)
	}
	if len(gh.postedBodies) != 0 {
		t.Errorf("PR comments = %q, want none on a race", gh.postedBodies)
	}
}

// TestProcessMergeGH_RebaseCleanStillPushes: a behind-but-mergeable branch
// (non-overlapping change) still takes the rebase + lease-push leg — the
// fast path is for refs that already contain the base, not a general skip.
func TestProcessMergeGH_RebaseCleanStillPushes(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "rebase")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID
	admin = gitRepoRoot(t, worktree)

	// Diverge non-overlapping: main edits README, branch adds feature.txt.
	gitRun(t, admin, "checkout", "main")
	gitCommitFile(t, admin, "README.md", "# main advanced\n")
	gitRun(t, admin, "push", "origin", "main")
	gitCommitFile(t, worktree, "feature.txt", "branch only\n")
	gitRun(t, worktree, "push", "origin", entry.Branch)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}
	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskRow(t, pool, taskID); status != "done" {
		t.Fatalf("task = %s, want done", status)
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

	// Attempt 1: below cap → silent release to pending.
	if err := ProcessMergeGH(ctx, pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}
	if got := entryStatus(t, pool, entry.ID); got != "pending" {
		t.Fatalf("attempt 1: entry = %q, want released to pending", got)
	}
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 0 {
		t.Fatalf("attempt 1 notified: %q (below-cap release must stay silent)", texts)
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
	if len(texts) != 1 {
		t.Fatalf("attempt 2 emitted %d notes, want 1", len(texts))
	}
	for _, want := range []string{"🆘", "CI failed 2 times", "parked needs-human", "maquinista approve " + taskID} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("note %q missing %q", texts[0], want)
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
