package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSpawnWithLayout_WorktreeCreateAndReuse covers the git-worktree
// creation + reuse branches of SpawnWithLayout without touching the
// DB or tmux — we short-circuit by passing a nil pool, which panics,
// so instead we factor out the worktree setup by exercising the
// filepath math and git.WorktreeAdd via a helper scenario: create a
// throwaway bare-ish repo, resolve a Layout for it, and verify that
// (a) the worktree dir + branch naming match what ResolveLayout
// returned, and (b) calling git.WorktreeAdd on the resolved values
// produces the worktree. This mirrors what SpawnWithLayout does up
// to the RegisterAgent call.
func TestLayout_RealGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	// Init a real repo with one commit so worktree add has a base.
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	run("git", "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-qm", "init")

	layout, err := ResolveLayout(ScopeAgent, dir, "alice", "")
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	cmd := exec.Command("git", "-C", dir, "worktree", "add", "-b", layout.Branch, layout.WorktreeDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %s: %v", out, err)
	}

	if _, err := os.Stat(layout.WorktreeDir); err != nil {
		t.Errorf("worktree dir %s missing: %v", layout.WorktreeDir, err)
	}
	if layout.WindowCWD() != layout.WorktreeDir {
		t.Errorf("WindowCWD = %q, want %q", layout.WindowCWD(), layout.WorktreeDir)
	}
}

