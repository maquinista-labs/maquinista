package pipeline

// Tests for the merge-leg build gate (MAQ-20). The gate materializes the
// pushed branch in a detached throwaway worktree and compiles it with the
// REAL Go toolchain (tiny stdlib-only modules — fast, no network), because
// the acceptance test must prove a branch that redeclares an existing
// symbol cannot be squash-merged. GitHub is faked at the GhRunner seam, as
// in merge_test.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// gateGoMod is a stdlib-only module: `go build ./...` never touches the
// network and compiles in well under a second warm.
const gateGoMod = "module maqgate\n\ngo 1.21\n"

// gateMain is main's copy of the symbol; gateDup redeclares it on the
// branch — the exact PR-#21 failure shape (duplicate const across files,
// invisible in a per-file diff read).
const gateMain = "package main\n\nconst GateSym = \"main\"\n\nfunc main() { _ = GateSym }\n"

const gateDup = "package main\n\nconst GateSym = \"branch\"\n\nvar _ = GateSym\n"

const gateClean = "package main\n\nconst ExtraSym = \"branch-only\"\n\nfunc helper() { _ = ExtraSym }\n"

// gateRedTest / gatePassingTest: the test leg's inputs — a branch whose test
// fails (deterministic red) and one whose test passes.
const gateRedTest = "package main\n\nimport \"testing\"\n\nfunc TestGateMustNotPass(t *testing.T) { t.Fatal(\"test gate rejects this branch\") }\n"

const gatePassingTest = "package main\n\nimport \"testing\"\n\nfunc TestGatePasses(t *testing.T) {\n\tif GateSym != \"main\" {\n\t\tt.Fatal(\"unexpected symbol\")\n\t}\n}\n"

// gateSub / gateSubTest: a second package used to prove deleted packages
// drop out of the test-leg selection.
const gateSub = "package sub\n\nconst SubSym = \"s\"\n"

const gateSubTest = "package sub\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) { _ = SubSym }\n"

// pushGoBase gives main a compiling Go module (after initRemoteTrio, whose
// branch was cut from the pre-module tip — the rebase in the merge flow is
// what brings go.mod onto the branch, exactly as in production).
func pushGoBase(t *testing.T, admin string) {
	t.Helper()
	gitCommitFile(t, admin, "go.mod", gateGoMod)
	gitCommitFile(t, admin, "main.go", gateMain)
	gitRun(t, admin, "push", "origin", "main")
}

// assertNoGateLeak is the acceptance leak check: no throwaway gate worktree
// survives on disk or in the repo's worktree registry — on success AND
// failure paths.
func assertNoGateLeak(t *testing.T, admin string) {
	t.Helper()
	// The suite may be RUNNING INSIDE a gate tree (every gate on a
	// pipeline-touching branch runs the package from the gate's own
	// materialized clone — see gateTree). That host tree is alive for the
	// whole run and is not a leak; anything else under a gate prefix is.
	// Skipping only the cwd's ancestor chain keeps the check honest: a
	// tree abandoned by an earlier failed run is never an ancestor of the
	// current cwd.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	isHost := func(p string) bool { return p == cwd || strings.HasPrefix(cwd, p+string(filepath.Separator)) }
	for _, prefix := range []string{mergeGateDirPrefix, gateCloneDirPrefix} {
		var leaks []string
		if leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), prefix+"*")); len(leftovers) != 0 {
			for _, l := range leftovers {
				if !isHost(l) {
					leaks = append(leaks, l)
				}
			}
		}
		if len(leaks) != 0 {
			t.Errorf("gate worktree(s) leaked under %s: %v", os.TempDir(), leaks)
		}
	}
	if out := gitRun(t, admin, "worktree", "list", "--porcelain"); strings.Contains(out, mergeGateDirPrefix) || strings.Contains(out, gateCloneDirPrefix) {
		t.Errorf("gate worktree still registered in the repo:\n%s", out)
	}
}

// TestGateTree_InvisibleToLeakChecks (MAQ-29/30 regression): the gate used
// to materialize its tree as a worktree of the TASK repo, under a
// mergegate prefix in TempDir — so when the pipeline suite ran INSIDE that
// tree (every gate on a pipeline-touching branch), the leak checks saw the
// gate's own crime scene and went red: five consecutive gate reds while
// every manual run of the same commits passed. The gate must materialize
// from a throwaway shared clone under its own prefix — invisible to both
// checks until cleanup removes it.
func TestGateTree_InvisibleToLeakChecks(t *testing.T) {
	admin, worktree := initRemoteTrio(t, "gatetree")
	pushGoBase(t, admin)

	dir, cleanup, err := gateTree(worktree, "origin/main")
	if err != nil {
		t.Fatalf("gateTree: %v", err)
	}
	defer cleanup()

	// The materialized tree must NOT be a mergegate-prefixed path in
	// TempDir, and must NOT be registered in the task repo's worktree
	// registry — both are things the suite asserts on while running inside
	// the gate.
	if leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), mergeGateDirPrefix+"*")); len(leftovers) != 0 {
		t.Errorf("gate tree visible to the mergegate leak glob: %v", leftovers)
	}
	if out := gitRun(t, admin, "worktree", "list", "--porcelain"); strings.Contains(out, dir) {
		t.Errorf("gate tree registered in the task repo's worktree registry:\n%s", out)
	}
	// ...and it is the tree it claims to be: go.mod present, at origin/main.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Errorf("gate tree has no go.mod: %v", err)
	}
	want := gitRun(t, admin, "rev-parse", "origin/main")
	if got := gitRun(t, dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("gate tree at %s, want origin/main %s", got, want)
	}

	// Cleanup removes the whole clone root — nothing left to leak.
	root := filepath.Dir(dir)
	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("clone root %s still exists after cleanup: %v", root, err)
	}
	assertNoGateLeak(t, admin)
}

// TestProcessMergeGH_BuildGateBlocksBrokenBranch (MAQ-20 acceptance #1, #2,
// #3, #4): a branch that redeclares an existing symbol must not reach
// `gh pr merge --squash`; the entry fails, the task routes back to the
// fixer (changes_requested — fixer round 1), and both the notification and
// the task observation carry the compiler output.
func TestProcessMergeGH_BuildGateBlocksBrokenBranch(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "buildgate")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	// main gets a compiling module; the branch redeclares GateSym.
	pushGoBase(t, admin)
	gitCommitFile(t, worktree, "dup.go", gateDup)
	gitRun(t, worktree, "push", "origin", entry.Branch)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}

	if gh.mergeCalls != 0 {
		t.Errorf("squash-merge fired %d times on a branch that does not compile", gh.mergeCalls)
	}
	if got := entryStatus(t, pool, entry.ID); got != "failed" {
		t.Errorf("entry status = %q, want failed", got)
	}
	if status, _ := taskRow(t, pool, taskID); status != "changes_requested" {
		t.Errorf("task status = %q, want changes_requested (fixer round 1)", status)
	}
	// Not merged: the remote branch survives for the fix + re-approve.
	if out := gitRun(t, admin, "ls-remote", "--heads", "origin", entry.Branch); out == "" {
		t.Error("branch vanished from the remote — the gate must not clean up work it rejected")
	}

	// The question carries the compiler output (acceptance #2).
	texts := pipelineNotifyTextsPool(t, pool)
	if len(texts) != 1 {
		t.Fatalf("emitted %d notes, want 1: %q", len(texts), texts)
	}
	for _, want := range []string{"🆘", "build gate failed", "go build ./...", "redeclared", "Back to the fixer", "round 1 of 3"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("note %q missing %q", texts[0], want)
		}
	}
	// The observation carries the same errors for downstream tasks.
	obs := taskContextTexts(t, pool, taskID, "merger")
	found := false
	for _, o := range obs {
		if strings.Contains(o, "Build gate failed") && strings.Contains(o, "redeclared") {
			found = true
		}
	}
	if !found {
		t.Errorf("no build-gate observation with compiler output, got %q", obs)
	}

	// The throwaway worktree is gone on the failure path too (acceptance #3).
	assertNoGateLeak(t, admin)
	// ...and the task worktree stays for the fix (only the gate's own
	// throwaway is cleaned up).
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("task worktree %s must survive a build-gate park: %v", worktree, err)
	}
}

// TestProcessMergeGH_BuildGatePassesCleanBranch: a compiling branch merges
// as before — the gate adds latency, never a dead end.
func TestProcessMergeGH_BuildGatePassesCleanBranch(t *testing.T) {
	pool := testPool(t)
	admin, worktree := initRemoteTrio(t, "gateok")
	entry := seedReadyTask(t, pool, worktree)
	taskID := entry.TaskID

	pushGoBase(t, admin)
	gitCommitFile(t, worktree, "extra.go", gateClean)
	gitRun(t, worktree, "push", "origin", entry.Branch)

	gh := &fakeGh{checks: ChecksGreen}
	cfg := MergeConfig{Mode: MergeModeGH, AutoMerge: true, Gh: gh}

	if err := ProcessMergeGH(context.Background(), pool, cfg, &fakeProvider{}, "team-1", entry); err != nil {
		t.Fatal(err)
	}

	if gh.mergeCalls != 1 {
		t.Fatalf("merge calls = %d, want 1 (clean branch must pass the gate)", gh.mergeCalls)
	}
	if status, prState := taskRow(t, pool, taskID); status != "done" || prState != "merged" {
		t.Errorf("task = %s/%s, want done/merged", status, prState)
	}
	if got := entryStatus(t, pool, entry.ID); got != "merged" {
		t.Errorf("entry status = %q, want merged", got)
	}
	assertNoGateLeak(t, admin)
}

// TestRunBuildGate_VacuousWithoutGoMod: non-Go branches pass without paying
// for a build (this is also what keeps the README-only merge_test trio
// green through the gate).
func TestRunBuildGate_VacuousWithoutGoMod(t *testing.T) {
	repo := initGateRepo(t, map[string]string{"README.md": "# no go here\n"})
	passed, _, err := runBuildGate(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("vacuous gate: %v", err)
	}
	if !passed {
		t.Error("non-Go branch must pass vacuously")
	}
	assertNoGateLeak(t, repo)
}

// TestRunBuildGate_InfraTroubleFailsEntry: a missing toolchain is an
// infrastructure error (distinct from a compile rejection) and still cleans
// up its worktree.
func TestRunBuildGate_InfraTroubleFailsEntry(t *testing.T) {
	repo := initGateRepo(t, map[string]string{
		"go.mod":  gateGoMod,
		"main.go": gateMain,
	})
	old := buildGateCmd
	// Bare name: the production failure mode is `go` missing from PATH,
	// which surfaces as exec.ErrNotFound from the LookPath probe.
	buildGateCmd = []string{"definitely-not-go-anywhere"}
	defer func() { buildGateCmd = old }()

	_, _, err := runBuildGate(context.Background(), repo, "HEAD")
	if err == nil {
		t.Fatal("missing toolchain must be an infra error, not a pass")
	}
	if !errors.Is(err, exec.ErrNotFound) && !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("err = %v, want a not-found flavor", err)
	}
	assertNoGateLeak(t, repo)
}

// TestRunBuildGate_CompilesDetachedHead: the gate checks out the ref
// content, not the task worktree's dirty state — a stray uncommitted file
// in the worktree must not leak into the build.
func TestRunBuildGate_CompilesDetachedHead(t *testing.T) {
	repo := initGateRepo(t, map[string]string{
		"go.mod":  gateGoMod,
		"main.go": gateMain,
	})
	// Uncommitted garbage in the source tree — the gate builds HEAD's tree,
	// so this must be invisible to it.
	if err := os.WriteFile(filepath.Join(repo, "broken.go"), []byte(gateDup), 0644); err != nil {
		t.Fatal(err)
	}
	passed, _, err := runBuildGate(context.Background(), repo, "HEAD")
	if err != nil || !passed {
		t.Fatalf("gate built something other than the committed tree: passed=%v err=%v", passed, err)
	}
	assertNoGateLeak(t, repo)
}

// TestTrimLines: the payload carries the FIRST ~20 lines, with an ellipsis
// count when truncated.
func TestTrimLines(t *testing.T) {
	if got := trimLines("a\nb\n", 20); got != "a\nb" {
		t.Errorf("short input: got %q", got)
	}
	long := strings.Repeat("line\n", 30)
	got := trimLines(long, 20)
	lines := strings.Split(got, "\n")
	if len(lines) != 21 || !strings.HasPrefix(lines[20], "… (+10 more lines)") {
		t.Errorf("truncated output = %d lines, tail %q; want 20 + ellipsis", len(lines), lines[len(lines)-1])
	}
	if !strings.HasPrefix(got, "line\n") {
		t.Errorf("truncation must keep the FIRST lines, got %q", lines[0])
	}
}

// TestTrimTailLines: the test-gate payload carries the LAST ~20 lines —
// go test prints its --- FAIL blocks at the very end, after all the suites'
// log output, so keeping the head buries the failure under the ellipsis
// (2026-10-06 merge gates: 20 lines of log noise, failure unseen).
func TestTrimTailLines(t *testing.T) {
	if got := trimTailLines("a\nb\n", 20); got != "a\nb" {
		t.Errorf("short input: got %q", got)
	}
	long := strings.Repeat("noise\n", 30) + "--- FAIL: TestGateMustNotPass\nFAIL"
	got := trimTailLines(long, 3)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "… (-29 earlier lines)") {
		t.Errorf("truncated output = %d lines, head %q; want ellipsis + 3 lines", len(lines), lines[0])
	}
	wantTail := "--- FAIL: TestGateMustNotPass\nFAIL"
	if !strings.HasSuffix(got, wantTail) {
		t.Errorf("tail trim must keep the LAST lines; got tail %q", got[len(got)-60:])
	}
}

// ---- test leg (MAQ-21) ----

// gateRepoAtBase commits files, returns the base SHA, then commits more.
func gateRepoAtBase(t *testing.T, base, branch map[string]string) (repo, baseRef string) {
	t.Helper()
	repo = initGateRepo(t, base)
	baseRef = gitRun(t, repo, "rev-parse", "HEAD")
	paths := make([]string, 0, len(branch))
	for p := range branch {
		paths = append(paths, p)
	}
	sortStrings(paths)
	for _, p := range paths {
		gitCommitFile(t, repo, p, branch[p])
	}
	return repo, baseRef
}

// TestRunTestGate_RedTestFails: a branch whose own test fails is a
// deterministic red — passed=false with the failing test named in the output
// and the exact `go test` command returned for the payload.
func TestRunTestGate_RedTestFails(t *testing.T) {
	repo, base := gateRepoAtBase(t,
		map[string]string{"go.mod": gateGoMod, "main.go": gateMain},
		map[string]string{"red_test.go": gateRedTest})

	passed, gateCmd, out, err := runTestGate(context.Background(), repo, "HEAD", base)
	if err != nil {
		t.Fatalf("red test must be deterministic, not infra: %v", err)
	}
	if passed {
		t.Fatal("a failing test passed the gate")
	}
	if gateCmd != "go test ." {
		t.Errorf("gateCmd = %q, want `go test .`", gateCmd)
	}
	for _, want := range []string{"--- FAIL: TestGateMustNotPass", "test gate rejects this branch"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	assertNoGateLeak(t, repo)
}

// TestRunTestGate_PassingTestMerges: a green test costs the run and passes.
func TestRunTestGate_PassingTestPasses(t *testing.T) {
	repo, base := gateRepoAtBase(t,
		map[string]string{"go.mod": gateGoMod, "main.go": gateMain},
		map[string]string{"ok_test.go": gatePassingTest})

	passed, gateCmd, out, err := runTestGate(context.Background(), repo, "HEAD", base)
	if err != nil || !passed {
		t.Fatalf("passing test must pass: passed=%v err=%v out=%q", passed, err, out)
	}
	if gateCmd != "go test ." {
		t.Errorf("gateCmd = %q, want `go test .`", gateCmd)
	}
	assertNoGateLeak(t, repo)
}

// TestRunTestGate_PackageSelection: only touched Go packages run; docs-only
// branches are vacuous; go.mod/go.sum shifts widen to the whole module; a
// package the branch DELETES drops out of the selection (testing a vanished
// directory would red-flag a legitimate removal), as does a directory kept
// alive by a non-Go file after its last .go file is deleted (`go test`
// there hard-fails "no Go files" — the round-2 false-red shape).
func TestRunTestGate_PackageSelection(t *testing.T) {
	t.Run("docs only is vacuous", func(t *testing.T) {
		repo, base := gateRepoAtBase(t,
			map[string]string{"go.mod": gateGoMod, "main.go": gateMain},
			map[string]string{"README.md": "# docs\n"})
		passed, gateCmd, _, err := runTestGate(context.Background(), repo, "HEAD", base)
		if err != nil || !passed {
			t.Fatalf("docs-only branch must pass vacuously: %v", err)
		}
		if gateCmd != "" {
			t.Errorf("vacuous pass must not name a command, got %q", gateCmd)
		}
		assertNoGateLeak(t, repo)
	})

	t.Run("go.mod widens to the module", func(t *testing.T) {
		repo, base := gateRepoAtBase(t,
			map[string]string{"go.mod": gateGoMod, "main.go": gateMain},
			map[string]string{"go.mod": gateGoMod + "\n"})
		passed, gateCmd, _, err := runTestGate(context.Background(), repo, "HEAD", base)
		if err != nil || !passed {
			t.Fatalf("go.mod-only branch must pass: %v", err)
		}
		if gateCmd != "go test ./..." {
			t.Errorf("gateCmd = %q, want the whole module", gateCmd)
		}
		assertNoGateLeak(t, repo)
	})

	t.Run("deleted package drops out", func(t *testing.T) {
		repo, base := gateRepoAtBase(t,
			map[string]string{
				"go.mod":          gateGoMod,
				"main.go":         gateMain,
				"sub/sub.go":      gateSub,
				"sub/sub_test.go": gateSubTest,
			},
			map[string]string{"ok_test.go": gatePassingTest})
		// The branch deletes sub/ entirely; the selection must shrink to the
		// surviving package.
		gitRun(t, repo, "rm", "-q", "-r", "sub")
		gitRun(t, repo, "commit", "-q", "-m", "delete sub")

		passed, gateCmd, _, err := runTestGate(context.Background(), repo, "HEAD", base)
		if err != nil || !passed {
			t.Fatalf("deleting a package must not red-flag the branch: %v", err)
		}
		if gateCmd != "go test ." {
			t.Errorf("gateCmd = %q, want only the surviving package", gateCmd)
		}
		assertNoGateLeak(t, repo)
	})

	t.Run("directory kept alive by a non-Go file drops out", func(t *testing.T) {
		repo, base := gateRepoAtBase(t,
			map[string]string{
				"go.mod":        gateGoMod,
				"main.go":       gateMain,
				"sub/sub.go":    gateSub,
				"sub/README.md": "# survives the deletion\n",
			},
			map[string]string{"ok_test.go": gatePassingTest})
		// The branch deletes the package's LAST .go file but the directory
		// survives on the README: os.Stat on the directory would keep ./sub
		// selected and `go test ./sub` would hard-fail [setup failed] — a
		// permanent false red on a legitimate removal. The selection must
		// shrink to the surviving package instead.
		gitRun(t, repo, "rm", "-q", "sub/sub.go")
		gitRun(t, repo, "commit", "-q", "-m", "delete last go file, keep dir")

		passed, gateCmd, _, err := runTestGate(context.Background(), repo, "HEAD", base)
		if err != nil || !passed {
			t.Fatalf("emptying a package must not red-flag the branch: %v", err)
		}
		if gateCmd != "go test ." {
			t.Errorf("gateCmd = %q, want only the surviving package", gateCmd)
		}
		assertNoGateLeak(t, repo)
	})

	t.Run("renderGateCmd caps the path wall", func(t *testing.T) {
		var pkgs []string
		for i := 0; i < 12; i++ {
			pkgs = append(pkgs, fmt.Sprintf("./p%02d", i))
		}
		got := renderGateCmd([]string{"go", "test"}, pkgs)
		if !strings.HasPrefix(got, "go test ./p00 ") || !strings.Contains(got, "… (+4 more)") {
			t.Errorf("renderGateCmd = %q, want capped list with a count", got)
		}
	})
}

// ---- helpers ----

// initGateRepo builds a minimal single-branch git repo for direct
// runBuildGate tests.
func initGateRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main", ".")
	gitRun(t, repo, "config", "user.email", "gate@test.com")
	gitRun(t, repo, "config", "user.name", "Gate")
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sortStrings(paths)
	for _, p := range paths {
		gitCommitFile(t, repo, p, files[p])
	}
	return repo
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func taskContextTexts(t *testing.T, pool *pgxpool.Pool, taskID, agentID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT content FROM task_context WHERE task_id = $1 AND agent_id = $2 ORDER BY id`,
		taskID, agentID)
	if err != nil {
		t.Fatalf("querying observations: %v", err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning observation: %v", err)
		}
		texts = append(texts, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("observation rows: %v", err)
	}
	return texts
}
