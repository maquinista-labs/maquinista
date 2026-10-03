package pipeline

// Merge quality gate. Leg 1 (build, MAQ-20): the merge leg used to
// squash-merge without ever compiling the branch — CI does not run on PRs,
// so nothing between the implementor verdict and `gh pr merge --squash`
// executed a build; PR #21 redeclared consts across two files and merged
// green, leaving main unbuildable. Leg 2 (tests, MAQ-21): decision C(b) —
// no squash without the test suite passing on the packages the branch
// touches (`go build ./...` does not compile _test.go files, so a removed
// test-only dependency or a red test slips past the build leg alone). Both
// legs materialize the pushed branch (origin/<branch> — the exact tree a
// squash-merge takes, rebase included) in a detached throwaway worktree.
// Deterministic failure: no merge — the entry fails, the task parks
// needs-human, and the question names the failing step and carries the
// first ~20 lines of output. The worktree is removed on every outcome.
// Deliberately no config knob: a gate that could be switched off in the
// default deployment would reintroduce the bug it exists to prevent.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// testGateTimeout caps the test leg the same way. Typical cost is the
	// touched packages only (most are seconds); this repo's heaviest
	// package (DB-backed integration tests) needs ~4min warm, so 10m is
	// insurance, not the expected spend. A timed-out run is infrastructure
	// trouble — never a deterministic rejection of the branch.
	testGateTimeout = 10 * time.Minute
	// maxBuildErrLines bounds the compiler/test output carried by the
	// needs-human notification and the task observation (MAQ-20: "first ~20
	// lines" — duplicate consts across files are invisible in a diff read
	// but loud in compiler output; a red test's first failure names itself
	// within the same budget).
	maxBuildErrLines = 20
	// maxGatePkgsRendered caps how many package paths the failing-step
	// command renders in the needs-human payload before collapsing the
	// rest into a count — the step must be nameable, not a wall of paths.
	maxGatePkgsRendered = 8
)

// testGateCmd is the command the test leg appends the touched packages to.
// A var so tests can substitute a stub; production always runs the stock Go
// toolchain from PATH.
var testGateCmd = []string{"go", "test"}

// buildGateStep / testGateStep name the two legs in payloads and logs.
const (
	buildGateStep = "build"
	testGateStep  = "test"
)

// buildGateCmd is the compile command the gate runs inside the materialized
// branch. A var only so tests can substitute a missing/failing binary;
// production always compiles with the stock Go toolchain from PATH.
var buildGateCmd = []string{"go", "build", "./..."}

// gateTree materializes ref (e.g. "origin/t-<id>/feature") in a detached
// throwaway worktree of the repo behind worktreeDir. The caller MUST call
// cleanup on every path (git remove first, then a raw rm + prune if git
// still balks — a killed build can leave locks); a leaked entry would
// accumulate full checkouts under /tmp.
func gateTree(worktreeDir, ref string) (dir string, cleanup func(), err error) {
	admin, err := git.CommonDir(worktreeDir)
	if err != nil {
		return "", nil, fmt.Errorf("gate tree: repo root: %w", err)
	}
	dir, err = os.MkdirTemp("", mergeGateDirPrefix)
	if err != nil {
		return "", nil, fmt.Errorf("gate tree: temp dir: %w", err)
	}
	// git worktree add wants a non-existent path; MkdirTemp reserved the
	// name, now yield it.
	if err := os.Remove(dir); err != nil {
		return "", nil, fmt.Errorf("gate tree: temp dir: %w", err)
	}
	done := false
	cleanup = func() {
		if done {
			return
		}
		done = true
		if rmErr := git.WorktreeRemove(admin, dir); rmErr != nil {
			os.RemoveAll(dir)
			_ = git.WorktreePrune(admin)
		}
	}
	if err := git.WorktreeAddDetached(admin, dir, ref); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("gate tree: materialize %s: %w", ref, err)
	}
	return dir, cleanup, nil
}

// runMergeGate runs both legs in order — compile, then tests on the touched
// packages. step/gateCmd/output identify the FAILING leg for the needs-human
// payload (step ∈ {build, test}); err is infrastructure trouble (the caller
// fails the queue entry — nothing merges, a re-approve retries).
func runMergeGate(ctx context.Context, worktreeDir, ref, baseRef string) (passed bool, step, gateCmd, output string, err error) {
	ok, out, err := runBuildGate(ctx, worktreeDir, ref)
	if err != nil || !ok {
		return ok, buildGateStep, strings.Join(buildGateCmd, " "), out, err
	}
	ok, cmd, out, err := runTestGate(ctx, worktreeDir, ref, baseRef)
	return ok, testGateStep, cmd, out, err
}

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
	dir, cleanup, err := gateTree(worktreeDir, ref)
	if err != nil {
		return false, "", fmt.Errorf("build gate: %w", err)
	}
	defer cleanup()
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

// runTestGate is the test leg (MAQ-21, decision C(b)): `go test` on the Go
// packages the branch touches (touchedPackages), inside its own materialized
// tree. Same outcome shapes as runBuildGate; gateCmd is the exact command
// the payload should name ("go test <pkgs>") — returned even on success so
// callers can assert package selection, empty on a vacuous pass (no Go code
// touched, or non-Go branch).
func runTestGate(ctx context.Context, worktreeDir, ref, baseRef string) (passed bool, gateCmd, output string, err error) {
	dir, cleanup, err := gateTree(worktreeDir, ref)
	if err != nil {
		return false, "", "", fmt.Errorf("test gate: %w", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		if !os.IsNotExist(err) {
			return false, "", "", fmt.Errorf("test gate: stat go.mod: %w", err)
		}
		// Non-Go branch: the build leg already passed vacuously.
		return true, "", "", nil
	}
	pkgs, err := touchedPackages(dir, baseRef)
	if err != nil {
		return false, "", "", fmt.Errorf("test gate: diff %s…%s: %w", baseRef, ref, err)
	}
	if len(pkgs) == 0 {
		// No Go code touched (docs-only branch): the build leg compiled
		// everything there is — nothing to test.
		return true, "", "", nil
	}
	gateCmd = renderGateCmd(testGateCmd, pkgs)

	testCtx, cancel := context.WithTimeout(ctx, testGateTimeout)
	defer cancel()
	args := append(append([]string{}, testGateCmd[1:]...), pkgs...)
	cmd := exec.CommandContext(testCtx, testGateCmd[0], args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, gateCmd, "", nil
	}
	exitErr := (*exec.ExitError)(nil)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return false, gateCmd, "", fmt.Errorf("test gate: %s not on PATH: %w", testGateCmd[0], err)
	case errors.Is(testCtx.Err(), context.DeadlineExceeded):
		return false, gateCmd, "", fmt.Errorf("test gate: timed out after %s", testGateTimeout)
	case testCtx.Err() != nil:
		return false, gateCmd, "", fmt.Errorf("test gate: cancelled: %w", testCtx.Err())
	case errors.As(err, &exitErr):
		// Deterministic red — the branch's fault, not infra's. The first
		// lines carry the failing test's name and assertion.
		return false, gateCmd, trimLines(string(out), maxBuildErrLines), nil
	default:
		return false, gateCmd, "", fmt.Errorf("test gate: run %v: %w: %s", cmd.Args, err, string(out))
	}
}

// touchedPackages maps the branch's changed files onto the Go packages the
// test leg must run: the directory of every changed .go file whose package
// still contains Go source in the tree, or the whole module (`./...`) when
// go.mod/go.sum changed — dependency shifts can break any package's tests
// while `go build ./...` stays green (test-only deps). Non-Go changes
// (docs, CI configs) select nothing. A package the branch DELETES drops
// out — testing a vanished directory would red-flag a legitimate removal —
// and so does a directory the branch emptied of its last .go file while a
// non-Go file keeps the directory alive: `go test` there hard-fails with
// "no Go files", a false deterministic red on a legitimate removal. Pure
// file mapping; the diff runs against the merge-base with baseRef so only
// the branch's own commits count, never base drift.
func touchedPackages(dir, baseRef string) ([]string, error) {
	mb, err := git.MergeBase(dir, baseRef, "HEAD")
	if err != nil {
		return nil, err
	}
	files, err := git.DiffNameOnly(dir, mb, "HEAD")
	if err != nil {
		return nil, err
	}
	moduleWide := false
	seen := map[string]bool{}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, ".go"):
			pkg := filepath.Dir(f)
			if pkg != "." {
				pkg = "./" + filepath.ToSlash(pkg)
			}
			// Only packages that still hold Go source (see above: a fully
			// deleted package drops out, and so does a directory whose last
			// .go file is gone but that a non-Go file keeps alive — the
			// directory surviving is NOT enough).
			if hasGoSource(dir, pkg) {
				seen[pkg] = true
			}
		case f == "go.mod" || f == "go.sum":
			moduleWide = true
		}
	}
	if moduleWide {
		return []string{"./..."}, nil
	}
	pkgs := make([]string, 0, len(seen))
	for p := range seen {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// hasGoSource reports whether pkg (as selected: "." or "./rel") still
// contains at least one .go file in the checked-out tree — the survival
// test for package selection in touchedPackages.
func hasGoSource(dir, pkg string) bool {
	pkgDir := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
	matches, err := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	return err == nil && len(matches) > 0
}

// renderGateCmd renders the failing-step command for human payloads: the
// toolchain invocation with at most maxGatePkgsRendered package paths, the
// rest collapsed into a count.
func renderGateCmd(cmd []string, pkgs []string) string {
	parts := append([]string{}, cmd...)
	if len(pkgs) <= maxGatePkgsRendered {
		parts = append(parts, pkgs...)
	} else {
		parts = append(parts, pkgs[:maxGatePkgsRendered]...)
		parts = append(parts, fmt.Sprintf("… (+%d more)", len(pkgs)-maxGatePkgsRendered))
	}
	return strings.Join(parts, " ")
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

// parkGateFailure records a gate rejection (MAQ-20 build / MAQ-21 test):
// the queue entry fails (terminal — the branch must change, so an immediate
// retry cannot succeed and re-approval after a fix enqueues a fresh entry),
// the task parks needs-human, and the human question names the failing step
// (AC 2: "the failing step named in the observation") and carries the first
// output lines so the fix needs no worktree spelunking.
func parkGateFailure(ctx context.Context, pool *pgxpool.Pool, taskID string, entry *db.MergeQueueEntry, step, gateCmd, output string) error {
	cause, outLabel, stepHead := "does not compile", "Compiler output", "Build"
	if step == testGateStep {
		cause, outLabel, stepHead = "fails", "Test output", "Test"
	}
	if err := db.FailMerge(pool, entry.ID, fmt.Sprintf("%s gate failed on branch %s: `%s` %s", step, entry.Branch, gateCmd, cause)); err != nil {
		return fmt.Errorf("pipeline: failing %d: %w", entry.ID, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks
		SET    status = 'pending_approval'
		WHERE  id = $1 AND status = 'ready_to_merge'
	`, taskID); err != nil {
		return fmt.Errorf("pipeline: parking %s after %s gate: %w", taskID, step, err)
	}
	db.AddObservation(pool, taskID, "merger",
		fmt.Sprintf("%s gate failed on branch %s — `%s` %s:\n%s", stepHead, entry.Branch, gateCmd, cause, output))
	notifyf(ctx, pool, "🆘 %s: %s gate failed on branch %s — `%s` %s. %s (first %d lines):\n%s\nTask parked needs-human. Fix, re-push, then `maquinista approve %s` to retry the merge.%s",
		taskTitle(ctx, pool, taskID), step, entry.Branch, gateCmd, cause, outLabel, maxBuildErrLines, output, taskID, prLinkSuffix(ctx, pool, taskID))
	log.Printf("pipeline: merge %s %s gate failed on branch %s", taskID, step, entry.Branch)
	return nil
}
