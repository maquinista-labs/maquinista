package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// testPool spins a disposable Postgres with the full migration chain applied.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return pool
}

// execOK is the local mustExec (the db package one is not importable).
func execOK(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func seedIssue(n int) Issue {
	return Issue{
		ID:          fmt.Sprintf("uuid-%d", n),
		Key:         fmt.Sprintf("MAQ-%d", 100+n),
		Title:       fmt.Sprintf("Do thing %d", n),
		Description: "the body",
		URL:         fmt.Sprintf("https://linear.app/maq/MAQ-%d", 100+n),
	}
}

func TestClaim_InsertsTaskRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(1)
	created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa", nil)
	if err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}
	if !created {
		t.Fatal("first claim should create")
	}

	var status, title, project string
	var meta map[string]string
	if err := pool.QueryRow(ctx, `
		SELECT status, title, project_id, metadata FROM tasks
		WHERE metadata->>'ticket_issue_id' = $1`, iss.ID,
	).Scan(&status, &title, &project, &meta); err != nil {
		t.Fatalf("task row: %v", err)
	}
	if status != "ready" {
		t.Errorf("status = %q, want ready (scheduler-claimable)", status)
	}
	if want := "[MAQ-101] Do thing 1"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	if project != "brisa" {
		t.Errorf("project_id = %q, want brisa", project)
	}
	if meta["ticket_url"] != iss.URL {
		t.Errorf("metadata.ticket_url = %q, want %q", meta["ticket_url"], iss.URL)
	}
}

func TestClaim_InsertsMapRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(2)
	if created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa", nil); err != nil || !created {
		t.Fatalf("ClaimIssue: created=%v err=%v", created, err)
	}

	var pend, last *string
	var attempts int
	if err := pool.QueryRow(ctx, `
		SELECT pending_state, last_synced_state, attempts
		FROM ticket_issue_map WHERE issue_id = $1`, iss.ID,
	).Scan(&pend, &last, &attempts); err != nil {
		t.Fatalf("map row: %v", err)
	}
	if pend == nil || *pend != ColInProgress.String() {
		t.Errorf("pending_state = %v, want %q", pend, ColInProgress.String())
	}
	if last != nil {
		t.Errorf("last_synced_state = %v, want NULL on a fresh claim", last)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
}

func TestClaim_Idempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(3)
	if _, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa", nil); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if created {
		t.Error("second claim should not create")
	}

	var tasks, maps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE metadata->>'ticket_issue_id' = $1`, iss.ID).Scan(&tasks); err != nil || tasks != 1 {
		t.Errorf("tasks rows for issue = %d (err=%v), want exactly 1", tasks, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ticket_issue_map WHERE issue_id = $1`, iss.ID).Scan(&maps); err != nil || maps != 1 {
		t.Errorf("map rows for issue = %d (err=%v), want exactly 1", maps, err)
	}
	var pend string
	if err := pool.QueryRow(ctx, `SELECT pending_state FROM ticket_issue_map WHERE issue_id = $1`, iss.ID).Scan(&pend); err != nil || pend != ColInProgress.String() {
		t.Errorf("map row changed on re-claim: pend=%q err=%v", pend, err)
	}
}

func TestClaim_SchedulerQueryMatches(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	iss := seedIssue(4)
	if _, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa", nil); err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}

	// The literal task-scheduler claim query (taskscheduler.go:94-105 shape).
	var taskID string
	var role *string
	err := pool.QueryRow(ctx, `
		SELECT id, metadata->>'role'
		FROM tasks t
		WHERE status = 'ready'
		  AND NOT EXISTS (
		        SELECT 1 FROM agents a
		        WHERE a.task_id = t.id AND a.status <> 'dead'
		      )
		ORDER BY priority DESC, created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&taskID, &role)
	if err != nil {
		t.Fatalf("scheduler query did not match the claimed task: %v", err)
	}
	var want string
	if err := pool.QueryRow(ctx, `SELECT id FROM tasks WHERE metadata->>'ticket_issue_id' = $1`, iss.ID).Scan(&want); err != nil {
		t.Fatalf("seeded task: %v", err)
	}
	if taskID != want {
		t.Errorf("scheduler claimed %q, want the bridge task %q", taskID, want)
	}
	if role != nil {
		t.Errorf("bridge task carries role %q, want NULL (EX-02 seeds souls)", *role)
	}
}

// --- MAQ-13: bridge-claimed tasks must land with a usable sibling worktree.

// initTestRepo builds a minimal git checkout with an origin/main remote
// tracking ref — enough for EnsureIssueWorktree to branch from origin/main.
func initTestRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@maquinista")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	// origin/main remote-tracking ref without a real remote.
	sha := strings.TrimSpace(string(mustOut(t, ctx, "git", "-C", dir, "rev-parse", "HEAD")))
	if out, err := exec.Command("git", "-C", dir, "update-ref", "refs/remotes/origin/main", sha).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v: %s", err, out)
	}
	return dir
}

func mustOut(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out
}

func TestSlugFromKey(t *testing.T) {
	cases := map[string]string{
		"MAQ-13":    "maq13",
		"BRISA-101": "brisa101",
		"abc":       "abc",
		"--":        "",
	}
	for in, want := range cases {
		if got := SlugFromKey(in); got != want {
			t.Errorf("SlugFromKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSiblingDir(t *testing.T) {
	got := SiblingDir("/home/u/code/maquinista", "maq13")
	want := "/home/u/code/maquinista.maq13"
	if got != want {
		t.Errorf("SiblingDir = %q, want %q", got, want)
	}
}

func TestEnsureIssueWorktree_CreatesFromOriginMain(t *testing.T) {
	repo := initTestRepo(t)
	iss := Issue{ID: "uuid-wt", Key: "MAQ-13", Title: "t"}

	wt, err := EnsureIssueWorktree(repo, iss)
	if err != nil {
		t.Fatalf("EnsureIssueWorktree: %v", err)
	}
	// House convention: sibling dir, never inside the repo.
	if want := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+".maq13"); wt != want {
		t.Errorf("worktree = %q, want %q", wt, want)
	}
	if fi, err := os.Stat(filepath.Join(wt, ".git")); err != nil || fi == nil {
		t.Fatalf("created path is not a worktree (no .git): %v", err)
	}
	branch := strings.TrimSpace(string(mustOut(t, context.Background(), "git", "-C", wt, "branch", "--show-current")))
	if branch != "maq13" {
		t.Errorf("branch = %q, want maq13", branch)
	}

	// Idempotent: second call reuses the same worktree.
	wt2, err := EnsureIssueWorktree(repo, iss)
	if err != nil || wt2 != wt {
		t.Errorf("reuse: wt2=%q err=%v, want %q", wt2, err, wt)
	}
}

func TestEnsureIssueWorktree_AttachesExistingBranch(t *testing.T) {
	repo := initTestRepo(t)
	// Branch exists (worktree was removed, branch kept).
	if out, err := exec.Command("git", "-C", repo, "branch", "maq7").CombinedOutput(); err != nil {
		t.Fatalf("branch: %v: %s", err, out)
	}
	wt, err := EnsureIssueWorktree(repo, Issue{ID: "u", Key: "MAQ-7"})
	if err != nil {
		t.Fatalf("EnsureIssueWorktree: %v", err)
	}
	branch := strings.TrimSpace(string(mustOut(t, context.Background(), "git", "-C", wt, "branch", "--show-current")))
	if branch != "maq7" {
		t.Errorf("branch = %q, want maq7 (attach)", branch)
	}
}

func TestClaim_SetsWorktreePath(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	repo := initTestRepo(t)
	iss := seedIssue(9)
	wt, err := EnsureIssueWorktree(repo, iss)
	if err != nil {
		t.Fatalf("EnsureIssueWorktree: %v", err)
	}
	created, err := ClaimIssue(ctx, pool, iss, "team-maq", "brisa", &wt)
	if err != nil || !created {
		t.Fatalf("ClaimIssue: created=%v err=%v", created, err)
	}
	var got *string
	if err := pool.QueryRow(ctx,
		`SELECT worktree_path FROM tasks WHERE metadata->>'ticket_issue_id' = $1`, iss.ID,
	).Scan(&got); err != nil {
		t.Fatalf("task row: %v", err)
	}
	if got == nil || *got != wt {
		t.Errorf("worktree_path = %v, want %q", got, wt)
	}
}
