package pipeline

// Sibling worktree provisioning for bridge-claimed tasks (MAQ-13): every
// bridged ticket must land with a usable worktree_path — the task scheduler
// refuses to spawn an implementor without one (EnsureAgent: "don't leak a
// half-spawned pane"), so a worktree-less claim loops forever. House
// convention, matching the manual incident fix this replaces:
//
//	dir    <parent>/<repoBase>.<slug>   e.g. ~/code/maquinista.maq13
//	branch <slug>                       e.g. maq13
//	base   origin/main                  (fallbacks: origin/HEAD, HEAD)
//
// The worktree is a sibling of the repo root, never inside it. Provisioning
// is best effort by design: a failure claims the task WITHOUT a worktree and
// the task scheduler parks it needs-human with one notification — the loud
// path, never a spawn wedge.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/maquinista-labs/maquinista/internal/git"
)

// WorktreeRepoEnv is the operator override for the repo root sibling
// worktrees are created from. Unset → the git root of the current working
// directory (the orchestrator runs from a checkout).
const WorktreeRepoEnv = "MAQUINISTA_TICKETS_REPO"

// WorktreeRepo returns the repo root worktrees are created from.
func WorktreeRepo() (string, error) {
	if r := strings.TrimSpace(os.Getenv(WorktreeRepoEnv)); r != "" {
		return r, nil
	}
	return git.RepoRoot(".")
}

// SlugFromKey derives the worktree/branch slug from a ticket key:
// "MAQ-13" → "maq13" (lowercase, [a-z0-9] kept). Empty when the key has no
// usable characters — callers must refuse to provision (git would mangle it).
func SlugFromKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SiblingDir is the house-convention path for slug's worktree: a sibling of
// repoRoot named <repoBase>.<slug> — never inside the repo.
func SiblingDir(repoRoot, slug string) string {
	return filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+"."+slug)
}

// EnsureIssueWorktree returns a usable sibling worktree for iss, creating it
// on first use (branch <slug> from origin/main). Idempotent: an existing
// worktree is reused, an existing branch is attached (worktree removed but
// branch kept). Refuses to touch a non-git directory at the target path.
func EnsureIssueWorktree(repoRoot string, iss Issue) (string, error) {
	slug := SlugFromKey(iss.Key)
	if slug == "" {
		return "", fmt.Errorf("pipeline: issue %s: no slug from key %q", iss.ID, iss.Key)
	}
	dir := SiblingDir(repoRoot, slug)

	// Reuse: linked worktrees carry a .git FILE pointing at the gitdir.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	}
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("pipeline: %s exists but is not a git worktree — refusing to touch it", dir)
	}

	base := "origin/main"
	switch {
	case git.RefExists(repoRoot, base):
	case git.RefExists(repoRoot, "origin/HEAD"):
		base = "origin/HEAD"
	default:
		base = "HEAD"
	}

	if err := git.WorktreeAddFrom(repoRoot, dir, slug, base); err != nil {
		// Branch already exists (leftover from a removed worktree, or the
		// task retried after a partial cleanup): attach it instead.
		if attachErr := git.WorktreeAttach(repoRoot, dir, slug); attachErr != nil {
			return "", fmt.Errorf("pipeline: worktree add %s (branch %s from %s): %v; attach fallback: %w", dir, slug, base, err, attachErr)
		}
	}
	return dir, nil
}
