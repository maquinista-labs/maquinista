package git

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ConflictError is returned when a merge has conflicts.
type ConflictError struct {
	Files []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("merge conflict in %d files: %s", len(e.Files), strings.Join(e.Files, ", "))
}

// RepoRoot returns the git repository root for the given directory.
func RepoRoot(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// CommonDir returns the repository's main directory (the one holding .git)
// for the given worktree or checkout — the right root for branch cleanup in
// linked-worktree setups, where RepoRoot is the worktree itself.
func CommonDir(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir in %s: %w", dir, err)
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Dir(p), nil
}

// CurrentBranch returns the current branch name for the given directory.
func CurrentBranch(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HasUncommittedChanges returns true if there are uncommitted changes in dir.
func HasUncommittedChanges(dir string) (bool, error) {
	err := exec.Command("git", "-C", dir, "diff", "--quiet", "HEAD").Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				return true, nil
			}
		}
		return false, fmt.Errorf("checking uncommitted changes in %s: %w", dir, err)
	}
	return false, nil
}

// HasUnmergedChanges returns true if branch has commits not in baseBranch.
func HasUnmergedChanges(dir, branch, baseBranch string) (bool, error) {
	out, err := exec.Command("git", "-C", dir, "log", "--oneline", baseBranch+".."+branch).Output()
	if err != nil {
		return false, fmt.Errorf("checking unmerged changes in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// AddAndCommit stages all changes and commits in the given directory.
// Returns the commit SHA, or empty string if nothing to commit.
func AddAndCommit(dir, message string) (string, error) {
	addCmd := exec.Command("git", "-C", dir, "add", "-A")
	if out, err := addCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add in %s: %s: %w", dir, strings.TrimSpace(string(out)), err)
	}

	if err := exec.Command("git", "-C", dir, "diff", "--cached", "--quiet").Run(); err == nil {
		return "", nil
	}

	commitCmd := exec.Command("git", "-C", dir, "commit", "-m", message)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit in %s: %s: %w", dir, strings.TrimSpace(string(out)), err)
	}

	shaCmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := shaCmd.Output()
	if err != nil {
		return "", fmt.Errorf("getting commit sha in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// WorktreeAdd creates a new worktree with a new branch.
func WorktreeAdd(repoRoot, worktreeDir, branch string) error {
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", branch, worktreeDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add -b %s %s: %s: %w", branch, worktreeDir, string(out), err)
	}
	return nil
}

// WorktreeRemove removes a worktree directory.
func WorktreeRemove(repoRoot, worktreeDir string) error {
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", worktreeDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove %s: %s: %w", worktreeDir, string(out), err)
	}
	return nil
}

// DeleteBranch deletes a local branch.
func DeleteBranch(repoRoot, branch string) error {
	cmd := exec.Command("git", "-C", repoRoot, "branch", "-D", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -D %s: %s: %w", branch, string(out), err)
	}
	return nil
}

// Fetch updates remote-tracking branches from the given remote.
func Fetch(dir, remote string) error {
	cmd := exec.Command("git", "-C", dir, "fetch", remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch %s in %s: %s: %w", remote, dir, string(out), err)
	}
	return nil
}

// SymbolicRefRemoteHead returns the default branch of the given remote
// (via the remote's HEAD symbolic ref), e.g. "main".
func SymbolicRefRemoteHead(dir, remote string) (string, error) {
	ref := "refs/remotes/" + remote + "/HEAD"
	cmd := exec.Command("git", "-C", dir, "symbolic-ref", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git symbolic-ref %s in %s: %w", ref, dir, err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/remotes/"+remote+"/"), nil
}

// RevParse returns the full SHA for a ref in the given directory.
func RevParse(dir, ref string) (string, error) {
	return revParse(dir, ref)
}

// Rebase rebases the current branch onto upstream (e.g. "origin/main").
// Returns the new HEAD SHA on success, or a *ConflictError if the rebase
// hits conflicts (the rebase is aborted before returning, leaving the
// worktree clean on the original branch).
func Rebase(dir, upstream string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rebase", upstream)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(out)
		if strings.Contains(outStr, "CONFLICT") || strings.Contains(outStr, "could not apply") {
			files := conflictFiles(dir)
			AbortRebase(dir)
			return "", &ConflictError{Files: files}
		}
		return "", fmt.Errorf("git rebase %s in %s: %s: %w", upstream, dir, outStr, err)
	}
	return revParse(dir, "HEAD")
}

// AbortRebase aborts an in-progress rebase (no-op when none is running).
func AbortRebase(dir string) error {
	cmd := exec.Command("git", "-C", dir, "rebase", "--abort")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git rebase --abort: %s: %w", string(out), err)
	}
	return nil
}

// PushForceWithLease force-pushes branch to remote with --force-with-lease,
// so the push fails if the remote moved underneath the rebase.
func PushForceWithLease(dir, branch, remote string) error {
	cmd := exec.Command("git", "-C", dir, "push", "--force-with-lease", remote, branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push --force-with-lease %s %s: %s: %w", remote, branch, string(out), err)
	}
	return nil
}

// DeleteRemoteBranch deletes a branch on the remote.
func DeleteRemoteBranch(dir, branch, remote string) error {
	cmd := exec.Command("git", "-C", dir, "push", remote, "--delete", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push %s --delete %s: %s: %w", remote, branch, string(out), err)
	}
	return nil
}

// MergeNoFF attempts a no-fast-forward merge. Returns the merge commit SHA on success,
// or a *ConflictError if there are conflicts.
func MergeNoFF(dir, branch, baseBranch, message string) (string, error) {
	checkout := exec.Command("git", "-C", dir, "checkout", baseBranch)
	if out, err := checkout.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git checkout %s: %s: %w", baseBranch, string(out), err)
	}

	mergeCmd := exec.Command("git", "-C", dir, "merge", "--no-ff", branch, "-m", message)
	out, err := mergeCmd.CombinedOutput()
	if err != nil {
		outStr := string(out)
		if strings.Contains(outStr, "CONFLICT") || strings.Contains(outStr, "Automatic merge failed") {
			files := conflictFiles(dir)
			return "", &ConflictError{Files: files}
		}
		return "", fmt.Errorf("git merge --no-ff %s: %s: %w", branch, outStr, err)
	}

	sha, err := revParse(dir, "HEAD")
	if err != nil {
		return "", err
	}
	return sha, nil
}

// MergeSquash performs a squash merge.
func MergeSquash(dir, branch, baseBranch, message string) (string, error) {
	checkout := exec.Command("git", "-C", dir, "checkout", baseBranch)
	if out, err := checkout.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git checkout %s: %s: %w", baseBranch, string(out), err)
	}

	squashCmd := exec.Command("git", "-C", dir, "merge", "--squash", branch)
	out, err := squashCmd.CombinedOutput()
	if err != nil {
		outStr := string(out)
		if strings.Contains(outStr, "CONFLICT") || strings.Contains(outStr, "Automatic merge failed") {
			files := conflictFiles(dir)
			return "", &ConflictError{Files: files}
		}
		return "", fmt.Errorf("git merge --squash %s: %s: %w", branch, outStr, err)
	}

	commitCmd := exec.Command("git", "-C", dir, "commit", "-m", message)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %s: %w", string(out), err)
	}

	sha, err := revParse(dir, "HEAD")
	if err != nil {
		return "", err
	}
	return sha, nil
}

// ListUnmergedBranches returns local branches not yet merged into the given base branch.
func ListUnmergedBranches(dir, baseBranch string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "branch", "--no-merged", baseBranch, "--format", "%(refname:short)")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git branch --no-merged %s: %w", baseBranch, err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

// ResetHard resets the working tree and index to HEAD.
func ResetHard(dir string) error {
	cmd := exec.Command("git", "-C", dir, "reset", "--hard", "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git reset --hard HEAD: %s: %w", string(out), err)
	}
	return nil
}

// AbortMerge aborts an in-progress merge.
func AbortMerge(dir string) error {
	cmd := exec.Command("git", "-C", dir, "merge", "--abort")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git merge --abort: %s: %w", string(out), err)
	}
	return nil
}

func conflictFiles(dir string) []string {
	cmd := exec.Command("git", "-C", dir, "diff", "--name-only", "--diff-filter=U")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func revParse(dir, ref string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}
