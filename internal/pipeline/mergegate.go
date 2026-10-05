package pipeline

// Build gate (MAQ-20). The merge leg used to squash-merge without ever
// compiling the branch: CI does not run on PRs, so nothing between the
// implementor verdict and `gh pr merge --squash` executed a build — PR #21
// redeclared consts across two files and merged green, leaving main
// unbuildable. The gate materializes the pushed branch (origin/<branch> —
// the exact tree a squash-merge takes, rebase included) in a detached
// throwaway worktree and runs `go build ./...` there. Compile failure: no
// merge — the entry fails, the task parks needs-human, and the question
// carries the first ~20 lines of compiler output. The worktree is removed
// on every outcome. Deliberately no config knob: a gate that could be
// switched off in the default deployment would reintroduce the bug it
// exists to prevent.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/git"
)

const (
	// mergeGateDirPrefix names the throwaway worktrees the gate creates
	// under os.TempDir() — also the leak-detection glob in tests.
	mergeGateDirPrefix = "maquinista-mergegate-"
	// buildGateTimeout caps a single gate build. The box compiles this repo
	// in ~60-90s warm (the added-latency budget); 10m is runaway insurance,
	// not a latency target. A timed-out build is infrastructure trouble
	// (entry failed, re-approvable) — it never merges unverified code, but
	// it also never blames the branch.
	buildGateTimeout = 10 * time.Minute
	// maxBuildErrLines bounds the compiler output carried by the needs-human
	// notification and the task observation (MAQ-20: "first ~20 lines" —
	// duplicate consts across files are invisible in a diff read but loud
	// in compiler output).
	maxBuildErrLines = 20
)

// buildGateCmd is the compile command the gate runs inside the materialized
// branch. A var only so tests can substitute a missing/failing binary;
// production always compiles with the stock Go toolchain from PATH.
var buildGateCmd = []string{"go", "build", "./..."}

// runBuildGate checks out ref (e.g. "origin/t-<id>/feature") in a detached
// temp worktree of the repo behind worktreeDir and compiles it. Outcomes:
//
//   - passed=true: compiles clean — or the branch root has no go.mod, in
//     which case the gate is vacuously green (non-Go repos merge as before).
//   - passed=false, output: deterministic compile failure; output is the
//     compiler's, trimmed to maxBuildErrLines.
//   - err: infrastructure trouble (worktree materialization failed,
//     toolchain missing, timeout). The caller fails the queue entry and
//     notifies — nothing merges, and a re-approve retries.
//
// The temp worktree is removed on EVERY path, success and failure alike;
// a leaked entry would accumulate full checkouts under /tmp.
func runBuildGate(ctx context.Context, worktreeDir, ref string) (passed bool, output string, err error) {
	admin, err := git.CommonDir(worktreeDir)
	if err != nil {
		return false, "", fmt.Errorf("build gate: repo root: %w", err)
	}
	dir, err := os.MkdirTemp("", mergeGateDirPrefix)
	if err != nil {
		return false, "", fmt.Errorf("build gate: temp dir: %w", err)
	}
	// git worktree add wants a non-existent path; MkdirTemp reserved the
	// name, now yield it.
	if err := os.Remove(dir); err != nil {
		return false, "", fmt.Errorf("build gate: temp dir: %w", err)
	}
	// Single cleanup point for every return below: git remove first, then a
	// raw rm + prune if git still balks (a killed build can leave locks).
	defer func() {
		if rmErr := git.WorktreeRemove(admin, dir); rmErr != nil {
			os.RemoveAll(dir)
			_ = git.WorktreePrune(admin)
		}
	}()

	if err := git.WorktreeAddDetached(admin, dir, ref); err != nil {
		return false, "", fmt.Errorf("build gate: materialize %s: %w", ref, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		if !os.IsNotExist(err) {
			return false, "", fmt.Errorf("build gate: stat go.mod: %w", err)
		}
		// Non-Go branch: nothing to compile — vacuous pass.
		return true, "", nil
	}

	buildCtx, cancel := context.WithTimeout(ctx, buildGateTimeout)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, buildGateCmd[0], buildGateCmd[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, "", nil
	}
	exitErr := (*exec.ExitError)(nil)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return false, "", fmt.Errorf("build gate: %s not on PATH: %w", buildGateCmd[0], err)
	case errors.Is(buildCtx.Err(), context.DeadlineExceeded):
		return false, "", fmt.Errorf("build gate: timed out after %s", buildGateTimeout)
	case buildCtx.Err() != nil:
		return false, "", fmt.Errorf("build gate: cancelled: %w", buildCtx.Err())
	case errors.As(err, &exitErr):
		// Deterministic compile failure — the branch's fault, not infra's.
		return false, trimLines(string(out), maxBuildErrLines), nil
	default:
		return false, "", fmt.Errorf("build gate: run %v: %w: %s", buildGateCmd, err, string(out))
	}
}

// trimLines keeps at most n lines of s, appending an ellipsis count when
// truncated (the needs-human payload carries the FIRST compiler lines).
func trimLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return fmt.Sprintf("%s\n… (+%d more lines)", strings.Join(lines[:n], "\n"), len(lines)-n)
}

// parkBuildFailure records a build-gate rejection (MAQ-20): the queue entry
// fails (terminal — the branch must change, so an immediate retry cannot
// succeed and re-approval after a fix enqueues a fresh entry), the task
// parks needs-human, and the human question carries the compiler output so
// the fix needs no worktree spelunking.
func parkBuildFailure(ctx context.Context, pool *pgxpool.Pool, taskID string, entry *db.MergeQueueEntry, output string) error {
	gateCmd := strings.Join(buildGateCmd, " ")
	if err := db.FailMerge(pool, entry.ID, fmt.Sprintf("build gate failed on branch %s: `%s` does not compile", entry.Branch, gateCmd)); err != nil {
		return fmt.Errorf("pipeline: failing %d: %w", entry.ID, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		SET    status = 'pending_approval'
		WHERE  id = $1 AND status = 'ready_to_merge'
	`, taskID); err != nil {
		return fmt.Errorf("pipeline: parking %s after build gate: %w", taskID, err)
	}
	db.AddObservation(pool, taskID, "merger",
		fmt.Sprintf("Build gate failed on branch %s — `%s` does not compile:\n%s", entry.Branch, gateCmd, output))
	notifyTaskf(ctx, pool, taskID, "🆘 %s: build gate failed on branch %s — %s does not compile. Compiler output (first %d lines):\n%s\nTask parked needs-human. Fix, re-push, then `maquinista approve %s` to retry the merge.%s",
		taskTitle(ctx, pool, taskID), entry.Branch, gateCmd, maxBuildErrLines, output, taskID, prLinkSuffix(ctx, pool, taskID))
	log.Printf("pipeline: merge %s build gate failed on branch %s", taskID, entry.Branch)
	return nil
}
