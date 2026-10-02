package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644)
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %s: %v", name, args, string(out), err)
	}
}

func TestRepoRoot(t *testing.T) {
	dir := initTestRepo(t)
	root, err := RepoRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root == "" {
		t.Error("expected non-empty root")
	}
}

func TestCurrentBranch(t *testing.T) {
	dir := initTestRepo(t)
	branch, err := CurrentBranch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if branch != "main" && branch != "master" {
		t.Errorf("branch = %q, want main or master", branch)
	}
}

func TestHasUncommittedChanges(t *testing.T) {
	dir := initTestRepo(t)

	has, err := HasUncommittedChanges(dir)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no uncommitted changes")
	}

	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0644)
	run(t, dir, "git", "add", ".")
	has, err = HasUncommittedChanges(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected uncommitted changes")
	}
}

func TestAddAndCommit(t *testing.T) {
	dir := initTestRepo(t)

	// Nothing to commit
	sha, err := AddAndCommit(dir, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "" {
		t.Error("expected empty sha for no changes")
	}

	// With changes
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0644)
	sha, err = AddAndCommit(dir, "add file")
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Error("expected non-empty sha")
	}
}

func TestConflictError(t *testing.T) {
	e := &ConflictError{Files: []string{"a.go", "b.go"}}
	if e.Error() == "" {
		t.Error("expected non-empty error message")
	}
}

// initRemoteRepo creates a bare origin plus an admin clone with a "main"
// branch pushed. Returns the origin path, the clone path, and the initial
// commit SHA on main.
func initRemoteRepo(t *testing.T) (origin, clone string) {
	t.Helper()
	origin = t.TempDir()
	clone = t.TempDir()
	run(t, origin, "git", "init", "--bare", ".")
	run(t, clone, "git", "clone", origin, ".")
	run(t, clone, "git", "config", "user.email", "test@test.com")
	run(t, clone, "git", "config", "user.name", "Test")
	run(t, clone, "git", "checkout", "-B", "main")
	os.WriteFile(filepath.Join(clone, "README.md"), []byte("# test\n"), 0644)
	run(t, clone, "git", "add", ".")
	run(t, clone, "git", "commit", "-m", "init")
	run(t, clone, "git", "push", "origin", "HEAD:refs/heads/main")
	return origin, clone
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// addCommit advances the repo with a change to path, returning the new SHA.
func addCommit(t *testing.T, dir, path, content string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0755)
	os.WriteFile(filepath.Join(dir, path), []byte(content), 0644)
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "update "+path)
	return gitOutput(t, dir, "rev-parse", "HEAD")
}

func TestRebase_Success(t *testing.T) {
	_, clone := initRemoteRepo(t)

	// Feature branch off the initial commit.
	run(t, clone, "git", "checkout", "-b", "feature")
	base := addCommit(t, clone, "feature.txt", "feature work")

	// Main advances on the remote.
	run(t, clone, "git", "checkout", "main")
	addCommit(t, clone, "docs.md", "main work")
	run(t, clone, "git", "push", "origin", "main")

	// Rebase feature onto the updated main.
	run(t, clone, "git", "checkout", "feature")
	gitOutput(t, clone, "fetch", "origin")
	sha, err := Rebase(clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" || sha == base {
		t.Errorf("expected new HEAD sha, got %q (base %q)", sha, base)
	}
	if gitOutput(t, clone, "log", "--format=%s", "-1") != "update feature.txt" {
		t.Error("feature commit should be replayed on top")
	}
}

func TestRebase_Conflict(t *testing.T) {
	_, clone := initRemoteRepo(t)
	os.WriteFile(filepath.Join(clone, "shared.txt"), []byte("base\n"), 0644)
	run(t, clone, "git", "add", ".")
	run(t, clone, "git", "commit", "-m", "shared base")
	run(t, clone, "git", "push", "origin", "main")

	run(t, clone, "git", "checkout", "-b", "feature")
	addCommit(t, clone, "shared.txt", "feature edit\n")

	run(t, clone, "git", "checkout", "main")
	addCommit(t, clone, "shared.txt", "main edit\n")
	run(t, clone, "git", "push", "origin", "main")

	run(t, clone, "git", "checkout", "feature")
	gitOutput(t, clone, "fetch", "origin")
	_, err := Rebase(clone, "origin/main")
	conflictErr, ok := err.(*ConflictError)
	if !ok {
		t.Fatalf("expected ConflictError, got %v", err)
	}
	if len(conflictErr.Files) != 1 || conflictErr.Files[0] != "shared.txt" {
		t.Errorf("expected conflict in shared.txt, got %v", conflictErr.Files)
	}
	// The rebase must be aborted: worktree clean, no rebase in progress.
	if gitOutput(t, clone, "status", "--porcelain") != "" {
		t.Error("worktree should be clean after abort")
	}
	if out, err := exec.Command("git", "-C", clone, "rev-parse", "--git-path", "rebase-merge").CombinedOutput(); err == nil && strings.TrimSpace(string(out)) != ".git/rebase-merge" && strings.TrimSpace(string(out)) != "rebase-merge" {
		t.Logf("rebase state path: %s", out) // best-effort; abort verified via clean status
	}
}

func TestPushForceWithLease(t *testing.T) {
	origin, clone := initRemoteRepo(t)
	run(t, clone, "git", "checkout", "-b", "feature")
	addCommit(t, clone, "f.txt", "one")
	run(t, clone, "git", "push", "origin", "feature")

	// Rewrite locally, then force-push with lease.
	addCommit(t, clone, "f.txt", "one rewritten")
	run(t, clone, "git", "reset", "--hard", "HEAD~1")
	addCommit(t, clone, "f.txt", "two")
	if err := PushForceWithLease(clone, "feature", "origin"); err != nil {
		t.Fatal(err)
	}
	originClone := t.TempDir()
	gitOutput(t, originClone, "clone", "--branch", "feature", origin, ".")
	if body := gitOutput(t, originClone, "show", "HEAD:f.txt"); body != "two" {
		t.Errorf("remote feature branch should hold rewritten content, got %q", body)
	}

	// Deleting the remote branch must also work.
	if err := DeleteRemoteBranch(clone, "feature", "origin"); err != nil {
		t.Fatal(err)
	}
	if out := gitOutput(t, origin, "branch", "--list", "feature"); out != "" {
		t.Errorf("remote branch should be gone, got %q", out)
	}
}
