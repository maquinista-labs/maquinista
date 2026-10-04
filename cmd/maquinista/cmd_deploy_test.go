package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

// ---- fixtures --------------------------------------------------------------

// initRuntimeRepo creates a fake runtime checkout (the files
// isRuntimeRepo keys on) plus a real git repo with a main branch.
func initRuntimeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{
		"Makefile",
		filepath.Join("cmd", "maquinista"),
		filepath.Join("internal", "db", "migrations"),
	} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "", "init", "--initial-branch=main", dir)
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

// runGit runs `git` with `-C dir` when dir != "" (empty dir = no -C, for
// commands that take the target path as an argument, like init/clone).
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := []string{}
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, "-c", "user.email=t@t", "-c", "user.name=T")
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}

// captureStdout redirects os.Stdout while fn runs and returns what was
// written. deploy prints via fmt.Print* which binds os.Stdout at call
// time, so swapping the var works.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = orig
	w.Close()
	return <-done
}

// ---- preflight tests -------------------------------------------------------

func TestDeployRepoDirResolvesByEnv(t *testing.T) {
	dir := initRuntimeRepo(t)
	t.Setenv("MAQUINISTA_DEPLOY_DIR", dir)
	got, err := deployRepoDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("deployRepoDir() = %q, want %q", got, dir)
	}
}

func TestDeployRepoDirFailsWithoutCheckout(t *testing.T) {
	t.Setenv("MAQUINISTA_DEPLOY_DIR", t.TempDir()) // exists but not a repo
	// Keep the remaining candidates away from real repos.
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if _, err := deployRepoDir(); err == nil {
		t.Error("expected error with no runtime checkout anywhere")
	}
}

func TestPreflightGitRefusesFeatureBranch(t *testing.T) {
	dir := initRuntimeRepo(t)
	runGit(t, dir, "checkout", "-b", "feature")
	dc := &deployCtx{repoDir: dir}
	err := dc.preflightGit()
	if err == nil || !strings.Contains(err.Error(), "deploy runs from main") {
		t.Fatalf("expected branch refusal, got %v", err)
	}
}

func TestPreflightGitRefusesUnpushedCommits(t *testing.T) {
	dir := initRuntimeRepo(t)
	// Stand up a bare origin, push main, then commit locally without pushing.
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	runGit(t, dir, "remote", "add", "origin", origin)
	runGit(t, dir, "push", "-u", "origin", "main")
	os.WriteFile(filepath.Join(dir, "unpushed.txt"), []byte("ship-blocker\n"), 0o644)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "not pushed yet")

	dc := &deployCtx{repoDir: dir}
	err := dc.preflightGit()
	if err == nil || !strings.Contains(err.Error(), "unpushed commit") {
		t.Fatalf("expected unpushed refusal, got %v", err)
	}
}

func TestPreflightGitAcceptsBehindOrigin(t *testing.T) {
	// Local main behind origin/main (playa pushed) is exactly the deploy case.
	dir := initRuntimeRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	runGit(t, dir, "remote", "add", "origin", origin)
	runGit(t, dir, "push", "-u", "origin", "main")

	// Push a second commit from a clone, so origin is ahead of local.
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", origin, clone)
	os.WriteFile(filepath.Join(clone, "merged.txt"), []byte("from playa\n"), 0o644)
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "merged on playa")
	runGit(t, clone, "push", "origin", "HEAD:main")

	dc := &deployCtx{repoDir: dir}
	if err := dc.preflightGit(); err != nil {
		t.Fatalf("preflightGit rejected a behind-main repo: %v", err)
	}
}

func TestPreflightGitRefusesDirtyTree(t *testing.T) {
	dir := initRuntimeRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	runGit(t, dir, "remote", "add", "origin", origin)
	runGit(t, dir, "push", "-u", "origin", "main")
	// Modify a TRACKED file — that's what breaks `git pull --rebase`.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# dirty\n"), 0o644)

	dc := &deployCtx{repoDir: dir}
	err := dc.preflightGit()
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("expected dirty-tree refusal, got %v", err)
	}
}

// ---- guard tests -----------------------------------------------------------

func TestIsBoxPane(t *testing.T) {
	fakeTmux := func(t *testing.T, session string, fail bool) {
		t.Helper()
		dir := t.TempDir()
		script := "#!/bin/sh\n"
		if fail {
			script += "exit 1\n"
		} else {
			script += "echo " + session + "\n"
		}
		p := filepath.Join(dir, "tmux")
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	}

	t.Run("outside tmux", func(t *testing.T) {
		t.Setenv("TMUX", "")
		if isBoxPane() {
			t.Error("no TMUX env → not a box pane")
		}
	})

	t.Run("inside orchestrator session", func(t *testing.T) {
		fakeTmux(t, "maquinista", false)
		t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
		t.Setenv("TMUX_SESSION_NAME", "")
		if !isBoxPane() {
			t.Error("tmux session maquinista → box pane")
		}
	})

	t.Run("inside unrelated session", func(t *testing.T) {
		fakeTmux(t, "dotfiles", false)
		t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
		if isBoxPane() {
			t.Error("unrelated tmux session → not a box pane")
		}
	})

	t.Run("tmux probe fails → conservative refusal", func(t *testing.T) {
		fakeTmux(t, "", true)
		t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
		if !isBoxPane() {
			t.Error("unknown tmux session → assume box pane")
		}
	})

	t.Run("session name override", func(t *testing.T) {
		fakeTmux(t, "maq-box", false)
		t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
		t.Setenv("TMUX_SESSION_NAME", "maq-box")
		if !isBoxPane() {
			t.Error("TMUX_SESSION_NAME=maq-box + matching session → box pane")
		}
	})
}

// ---- swap helpers ----------------------------------------------------------

func TestWaitProcsGoneImmediate(t *testing.T) {
	// /usr/bin/false exits 1 = "no matching process" → returns immediately.
	start := time.Now()
	if err := waitProcsGone("false", nil, 10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("waitProcsGone blocked despite immediate pgrep miss")
	}
}

func TestWaitProcsGoneTimeout(t *testing.T) {
	// `true` exits 0 (= "process found") instantly → loop until the
	// (short) timeout expires.
	err := waitProcsGone("true", nil, 50*time.Millisecond, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestCopyBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "next")
	dst := filepath.Join(dir, "maquinista")
	os.WriteFile(src, []byte("#!/bin/sh\necho new\n"), 0o755)
	os.WriteFile(dst, []byte("#!/bin/sh\necho old\n"), 0o755)

	if err := copyBinary(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "#!/bin/sh\necho new\n" {
		t.Errorf("dst not replaced: %q", got)
	}
	if _, err := os.Stat(dst + ".deploy-tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

// ---- health check ----------------------------------------------------------

func TestJournalErrorScan(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"Oct 03 18:00:00 box maquinista[1]: panic: runtime error: index out of range", true},
		{"Oct 03 18:00:00 box maquinista[1]: FATAL: database unreachable", true},
		{"Oct 03 18:00:00 box maquinista[1]: ERROR: relay send failed", true},
		{"Oct 03 18:00:00 box maquinista[1]: orchestrator started", false},
		{"Oct 03 18:00:00 box maquinista[1]: dashboard listening on 127.0.0.1:8900", false},
		{"Oct 03 18:00:00 box systemd[1]: Started maquinista.", false},
	}
	for _, c := range cases {
		if got := journalErrRe.MatchString(c.line); got != c.want {
			t.Errorf("journalErrRe(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// ---- plan ------------------------------------------------------------------

func TestDeployPlanListsAllSteps(t *testing.T) {
	dc := &deployCtx{repoDir: "/box/code/maquinista", unit: "maquinista", branch: "main", head: "abcdef1234567890"}
	out := captureStdout(t, func() { dc.printPlan() })
	for _, want := range []string{
		"/box/code/maquinista",
		"git pull --rebase origin main",
		"SKIP_DASHBOARD=1 make build-go",
		deployStagingName,
		"migrate",
		"systemctl stop maquinista",
		deployWaitProc,
		"systemctl start maquinista",
		"health check",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q\nplan:\n%s", want, out)
		}
	}
}

// ---- DB-backed mid-flight check --------------------------------------------

func TestLiveWorkerAgents(t *testing.T) {
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO tasks (id, title) VALUES ('t1','one'), ('t2','two')`)
	mustExec(t, pool, `
		INSERT INTO agents (id, tmux_session, tmux_window, task_id, status) VALUES
		('impl-1', 'maquinista', 'w1', 't1', 'working'),  -- mid-flight → reported
		('impl-2', 'maquinista', 'w2', 't2', 'dead'),     -- dead → ignored
		('idle-1', 'maquinista', 'w3', NULL, 'working')   -- not task-bound → ignored
	`)

	got, err := liveWorkerAgents(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "impl-1" {
		t.Errorf("liveWorkerAgents() = %v, want [impl-1]", got)
	}

	// Retire the worker → deploy becomes safe (swap between rounds).
	mustExec(t, pool, `UPDATE agents SET status='dead' WHERE id='impl-1'`)
	got, err = liveWorkerAgents(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after retire liveWorkerAgents() = %v, want empty", got)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

var _ = fmt.Sprintf // keep fmt if unused after edits
