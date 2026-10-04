package main

// maquinista deploy — internalizes scripts/deploy-barceloneta.sh as a
// first-class verb (MAQ-23). Runs ON the runtime box: pull what playa
// pushed, build staged, migrate, swap the binary in runbook §9 order
// (stop → wait pgrep clear → cp → start), then a 30s health check that
// gates the exit code. Otavio's auto-deploy-on-new-master-commit wiring
// can later invoke exactly this verb.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/spf13/cobra"
)

const (
	// deployStagingName is where the fresh binary lands. Building staged
	// keeps the swap atomic-ish and honors §9: the running binary is
	// only replaced after the unit is stopped (no ETXTBSY, no half-old
	// process serving new requests).
	deployStagingName = ".maquinista.deploy-next"
	// deployWaitProc is the systemd ExecStart signature the swap waits
	// on (runbook §9: stop → wait `pgrep -f 'maquinista orchestrator
	// start'` clear → cp → start).
	deployWaitProc = "maquinista orchestrator start"
	// deployHealthWindow is the post-start journal observation period.
	deployHealthWindow = 30 * time.Second
	// deployWaitTimeout caps how long the swap waits for the old
	// orchestrator process to exit after `systemctl stop`.
	deployWaitTimeout = 60 * time.Second
)

var (
	deployDryRun bool
	deployYes    bool
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy this checkout to the runtime: pull, build, migrate, swap binary, health check",
	Long: `Ship already-merged main changes to this box's maquinista runtime
(internalizes scripts/deploy-barceloneta.sh).

Preflight (read-only):
  - resolves the runtime checkout: MAQUINISTA_DEPLOY_DIR > dir of this
    binary > ~/code/maquinista > cwd
  - refuses on branch != main, uncommitted changes, or unpushed commits
  - refuses from inside a box tmux pane without --yes (the restart kills
    panes — the r1-suicide class)
  - refuses while task-bound agents are mid-flight without --yes (swaps
    are only safe between rounds)

Sequence: git pull --rebase origin main → SKIP_DASHBOARD=1 make build-go
(staged) → staged binary ` + "`migrate`" + ` → systemctl stop → wait for the
orchestrator process to exit → cp into place → systemctl start → health
check (unit active + panic/fatal/ERROR journal scan over the first 30s)
that gates the exit code.`,
	RunE: runDeploy,
}

func init() {
	deployCmd.Flags().BoolVar(&deployDryRun, "dry-run", false, "print the plan and preflight result without touching the box")
	deployCmd.Flags().BoolVar(&deployYes, "yes", false, "override the self-deploy and workers-mid-flight guards")
	rootCmd.AddCommand(deployCmd)
}

// deployCtx carries everything the plan/execute phases need.
type deployCtx struct {
	repoDir string
	unit    string
	branch  string
	head    string
}

func runDeploy(cmd *cobra.Command, args []string) error {
	repoDir, err := deployRepoDir()
	if err != nil {
		return err
	}
	// Load the repo's .env so DATABASE_URL is available for the
	// mid-flight workers check regardless of the caller's cwd. Existing
	// environment wins (godotenv never overrides).
	_ = godotenv.Load(filepath.Join(repoDir, ".env"))

	unit := os.Getenv("MAQUINISTA_DEPLOY_UNIT")
	if unit == "" {
		unit = "maquinista"
	}

	dc := &deployCtx{repoDir: repoDir, unit: unit}

	// ---- preflight ---------------------------------------------------------

	// Guard 1: self-deploy from a box pane (r1-suicide class).
	if isBoxPane() && !deployYes {
		return fmt.Errorf("refusing: running inside tmux session %q — the restart would kill this pane mid-deploy; re-run with --yes to deploy anyway",
			orchestratorSessionName())
	}

	// Guard 2: git state. Deploy ships PUSHED main only.
	if err := dc.preflightGit(); err != nil {
		return err
	}

	// Guard 3: workers mid-flight. Restart kills every tmux pane,
	// decapitating task-bound agents mid-task.
	if err := connectDB(); err != nil {
		if !deployYes {
			return fmt.Errorf("refusing: cannot check workers mid-flight (db: %v); fix DATABASE_URL or pass --yes", err)
		}
		fmt.Printf("WARN: db unreachable (%v) — skipping workers check (--yes)\n", err)
	} else {
		workers, err := liveWorkerAgents(context.Background(), pool)
		if err != nil {
			if !deployYes {
				return fmt.Errorf("refusing: workers check failed: %w (pass --yes to deploy anyway)", err)
			}
			fmt.Printf("WARN: workers check failed (%v) — continuing (--yes)\n", err)
		} else if len(workers) > 0 && !deployYes {
			return fmt.Errorf("refusing: %d worker(s) mid-flight (%s) — a restart would kill them; retry between rounds or pass --yes",
				len(workers), strings.Join(workers, ", "))
		} else if len(workers) > 0 {
			fmt.Printf("WARN: deploying with %d worker(s) mid-flight (%s) (--yes)\n", len(workers), strings.Join(workers, ", "))
		}
	}

	// ---- plan ---------------------------------------------------------------

	dc.printPlan()

	if deployDryRun {
		fmt.Println("(dry-run) nothing touched")
		return nil
	}

	// ---- execute ------------------------------------------------------------

	return dc.execute()
}

// preflightGit verifies branch == main, a clean tree, and that local
// main has no unpushed commits (deploy = ship what was PUSHED).
func (dc *deployCtx) preflightGit() error {
	branch, err := git.CurrentBranch(dc.repoDir)
	if err != nil {
		return fmt.Errorf("current branch: %w", err)
	}
	dc.branch = branch
	if branch != "main" {
		return fmt.Errorf("refusing: deploy runs from main, current branch is %q (cd %s && git checkout main)", branch, dc.repoDir)
	}

	dirty, err := git.HasUncommittedChanges(dc.repoDir)
	if err != nil {
		return fmt.Errorf("uncommitted check: %w", err)
	}
	if dirty {
		return fmt.Errorf("refusing: %s has uncommitted changes — commit or stash first (git pull --rebase would fail midway)", dc.repoDir)
	}

	if out, err := runCapture("git", "-C", dc.repoDir, "fetch", "origin", "main"); err != nil {
		return fmt.Errorf("refusing: cannot verify local main == origin/main (fetch failed: %v)\n%s", err, out)
	}
	head, err := git.RevParse(dc.repoDir, "HEAD")
	if err != nil {
		return fmt.Errorf("rev-parse HEAD: %w", err)
	}
	dc.head = head
	origin, err := git.RevParse(dc.repoDir, "origin/main")
	if err != nil {
		return fmt.Errorf("rev-parse origin/main: %w", err)
	}
	if head != origin {
		// Being BEHIND origin/main is fine — the pull step fast-forwards.
		// Only local commits origin hasn't seen are a refusal.
		n, err := gitRevListCount(dc.repoDir, "origin/main..main")
		if err != nil {
			return fmt.Errorf("refusing: local main (%s) != origin/main (%s) and rev-list failed: %v", short7(head), short7(origin), err)
		}
		if n > 0 {
			return fmt.Errorf("refusing: local main has %d unpushed commit(s) — deploy ships pushed main only (git push origin main first)", n)
		}
	}
	return nil
}

// printPlan renders the deployment plan (also the --dry-run output).
func (dc *deployCtx) printPlan() {
	fmt.Println("==> deploy plan")
	fmt.Printf("    repo:  %s (branch %s @ %s)\n", dc.repoDir, dc.branch, short7(dc.head))
	fmt.Printf("    unit:  %s\n", dc.unit)
	fmt.Printf("    steps:\n")
	fmt.Printf("      1. git pull --rebase origin main\n")
	fmt.Printf("      2. SKIP_DASHBOARD=1 make build-go (staged → %s)\n", deployStagingName)
	fmt.Printf("      3. %s migrate\n", deployStagingName)
	fmt.Printf("      4. sudo -n systemctl stop %s\n", dc.unit)
	fmt.Printf("      5. wait for %q processes to exit\n", deployWaitProc)
	fmt.Printf("      6. cp %s → %s\n", deployStagingName, dc.binaryPath())
	fmt.Printf("      7. sudo -n systemctl start %s\n", dc.unit)
	fmt.Printf("      8. health check: unit active + journal scan over first %s (gates exit code)\n", deployHealthWindow)
}

// execute runs the plan for real.
func (dc *deployCtx) execute() error {
	// 1. pull — fast-forward to what playa pushed.
	fmt.Printf("==> git pull --rebase origin main (was %s)\n", short7(dc.head))
	if out, err := runCaptureDir(dc.repoDir, "git", "pull", "--rebase", "origin", "main"); err != nil {
		return fmt.Errorf("pull --rebase: %w\n%s", err, out)
	}
	head, err := git.RevParse(dc.repoDir, "HEAD")
	if err != nil {
		return fmt.Errorf("rev-parse HEAD: %w", err)
	}
	commit, err := runCaptureDir(dc.repoDir, "git", "log", "-1", "--format=%s")
	if err != nil {
		commit = ""
	}
	fmt.Printf("==> now at %s %s\n", short7(head), strings.TrimSpace(commit))

	// 2. build staged (SKIP_DASHBOARD=1 — dashboard ships as standalone.tgz).
	staging := filepath.Join(dc.repoDir, deployStagingName)
	_ = os.Remove(staging)
	fmt.Println("==> building (SKIP_DASHBOARD=1 make build-go)")
	if out, err := dc.buildStaged(staging); err != nil {
		return fmt.Errorf("build: %w\n%s", err, out)
	}

	// 3. migrate — migrations only ever ADD files, so running the staged
	// binary before the swap is safe and idempotent.
	fmt.Println("==> migrating")
	if out, err := runCaptureDir(dc.repoDir, "./"+deployStagingName, "migrate"); err != nil {
		return fmt.Errorf("migrate: %w\n%s", err, out)
	}

	// 4–7. binary swap, runbook §9 order.
	fmt.Printf("==> swapping binary (stop → wait → cp → start)\n")
	startAt := time.Now()
	if out, err := runCapture("sudo", "-n", "systemctl", "stop", dc.unit); err != nil {
		return fmt.Errorf("stop %s: %w\n%s", dc.unit, err, out)
	}
	if err := waitOrchestratorExit(deployWaitTimeout); err != nil {
		// Best-effort: bring the unit back up on the old binary.
		_, _ = runCapture("sudo", "-n", "systemctl", "start", dc.unit)
		return fmt.Errorf("%w (attempted to restart %s on the old binary)", err, dc.unit)
	}
	if err := copyBinary(staging, dc.binaryPath()); err != nil {
		_, _ = runCapture("sudo", "-n", "systemctl", "start", dc.unit)
		return fmt.Errorf("swap: %w (attempted to restart %s on the old binary)", err, dc.unit)
	}
	if out, err := runCapture("sudo", "-n", "systemctl", "start", dc.unit); err != nil {
		return fmt.Errorf("start %s: %w\n%s", dc.unit, err, out)
	}
	_ = os.Remove(staging)

	// 8. health check — gates the exit code (criterion 1: exit 0 only on green).
	return dc.healthCheck(startAt, head)
}

// buildStaged runs SKIP_DASHBOARD=1 make build-go with the binary output
// redirected to `staging` (§9 keeps the running binary untouched until
// the swap). PATH is augmented with /usr/local/go/bin — go is not on the
// non-interactive ssh PATH on the box.
func (dc *deployCtx) buildStaged(staging string) (string, error) {
	cmd := exec.Command("make", "build-go", "BINARY="+staging)
	cmd.Dir = dc.repoDir
	cmd.Env = append(os.Environ(), "SKIP_DASHBOARD=1", "PATH="+augmentedPATH())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// healthCheck watches the unit for deployHealthWindow: the unit must be
// active after the window and the journal since startAt must be free of
// panic/fatal/ERROR lines (the script's grep, now gating). Any failure
// is a non-zero exit for the verb.
func (dc *deployCtx) healthCheck(startAt time.Time, head string) error {
	fmt.Printf("==> health check: watching %s for %s\n", dc.unit, deployHealthWindow)

	if err := waitUnitActive(dc.unit, 15*time.Second); err != nil {
		dc.dumpJournal(startAt)
		return err
	}

	// Sleep out the observation window (measured from the restart).
	if d := time.Until(startAt.Add(deployHealthWindow)); d > 0 {
		time.Sleep(d)
	}

	if err := exec.Command("systemctl", "is-active", "--quiet", dc.unit).Run(); err != nil {
		dc.dumpJournal(startAt)
		return fmt.Errorf("%s is not active %s after restart", dc.unit, deployHealthWindow)
	}

	lines, err := journalErrorLines(dc.unit, startAt)
	if err != nil {
		return fmt.Errorf("journal scan failed: %w", err)
	}
	if len(lines) > 0 {
		fmt.Printf("FAIL: error lines in the first %s of journal:\n", deployHealthWindow)
		for i, ln := range lines {
			if i >= 10 {
				fmt.Printf("  ... and %d more\n", len(lines)-10)
				break
			}
			fmt.Printf("  %s\n", ln)
		}
		return fmt.Errorf("health check failed: %d error line(s) in journal since restart", len(lines))
	}

	fmt.Printf("==> OK: %s active, deployed %s. Agents are reconciled from the DB on start.\n", dc.unit, short7(head))
	return nil
}

func (dc *deployCtx) dumpJournal(since time.Time) {
	out, err := runCapture("journalctl", "-u", dc.unit, "--since", fmt.Sprintf("@%d", since.Unix()), "--no-pager")
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	tail := lines
	if len(tail) > 30 {
		tail = tail[len(tail)-30:]
	}
	fmt.Println("recent logs:")
	for _, ln := range tail {
		fmt.Printf("  %s\n", ln)
	}
}

func (dc *deployCtx) binaryPath() string {
	return filepath.Join(dc.repoDir, "maquinista")
}

// ---- preflight helpers -----------------------------------------------------

// deployRepoDir resolves the runtime checkout this verb deploys.
// Precedence: MAQUINISTA_DEPLOY_DIR > dir of the running binary (on the
// box the verb runs from $REPO/maquinista per systemd ExecStart) >
// ~/code/maquinista > cwd.
func deployRepoDir() (string, error) {
	var candidates []string
	if d := os.Getenv("MAQUINISTA_DEPLOY_DIR"); d != "" {
		candidates = append(candidates, d)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "code", "maquinista"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	for _, c := range candidates {
		if isRuntimeRepo(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no runtime checkout found (tried %s); set MAQUINISTA_DEPLOY_DIR", strings.Join(candidates, ", "))
}

// isRuntimeRepo reports whether dir looks like the maquinista checkout
// (Makefile + cmd/maquinista + internal/db/migrations).
func isRuntimeRepo(dir string) bool {
	for _, p := range []string{"Makefile", filepath.Join("cmd", "maquinista"), filepath.Join("internal", "db", "migrations")} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			return false
		}
	}
	return true
}

// isBoxPane reports whether this process runs inside a tmux pane of the
// orchestrator session — a restart would kill the caller mid-deploy.
// Unknown tmux state counts as box pane (conservative).
func isBoxPane() bool {
	if os.Getenv("TMUX") == "" {
		return false
	}
	name, err := currentTmuxSession()
	if err != nil {
		return true // inside tmux but can't tell which session — assume the worst
	}
	return name == orchestratorSessionName()
}

func currentTmuxSession() (string, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func orchestratorSessionName() string {
	if s := strings.TrimSpace(os.Getenv("TMUX_SESSION_NAME")); s != "" {
		return s
	}
	return "maquinista"
}

// liveWorkerAgents returns agent ids that are task-bound and not dead —
// exactly the uq_agents_task_live predicate. These are the workers a
// restart would decapitate mid-task.
func liveWorkerAgents(ctx context.Context, p *pgxpool.Pool) ([]string, error) {
	rows, err := p.Query(ctx, `SELECT id FROM agents WHERE task_id IS NOT NULL AND status != 'dead' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---- swap helpers ----------------------------------------------------------

// waitOrchestratorExit polls pgrep until no `maquinista orchestrator
// start` process remains (§9 binary-swap order). pgrep exit 1 = none.
func waitOrchestratorExit(timeout time.Duration) error {
	return waitProcsGone("pgrep", []string{"-f", deployWaitProc}, time.Second, timeout)
}

func waitProcsGone(bin string, args []string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := exec.Command(bin, args...).Run()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return nil // no matching process — clear to swap
			}
			return fmt.Errorf("pgrep: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: %q processes still running after %s", deployWaitProc, timeout)
		}
		time.Sleep(interval)
	}
}

func copyBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".deploy-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst) // same dir → atomic replace
}

// ---- health helpers --------------------------------------------------------

// journalErrRe mirrors the deploy script's `grep -iE "panic|fatal|ERROR"`.
var journalErrRe = regexp.MustCompile(`(?i)panic|fatal|error`)

// journalErrorLines returns journal lines for `unit` since `since` that
// look like panics/fatals/errors.
func journalErrorLines(unit string, since time.Time) ([]string, error) {
	out, err := runCapture("journalctl", "-u", unit, "--since", fmt.Sprintf("@%d", since.Unix()), "--no-pager")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, ln := range strings.Split(out, "\n") {
		if journalErrRe.MatchString(ln) {
			lines = append(lines, ln)
		}
	}
	return lines, nil
}

func waitUnitActive(unit string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not active within %s after restart", unit, timeout)
		}
		time.Sleep(time.Second)
	}
}

// ---- small exec helpers ----------------------------------------------------

// augmentedPATH keeps go reachable in non-interactive ssh / cron contexts
// (go lives in /usr/local/go/bin on the box and is NOT on the default
// non-interactive PATH).
func augmentedPATH() string {
	path := os.Getenv("PATH")
	for _, want := range []string{"/usr/local/go/bin", "/usr/local/bin", "/usr/bin", "/bin"} {
		if !strings.Contains(path, want+":") && !strings.HasSuffix(path, want) {
			path = want + ":" + path
		}
	}
	return path
}

func runCapture(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func runCaptureDir(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitRevListCount counts commits in `spec` (e.g. origin/main..main).
func gitRevListCount(repoDir, spec string) (int, error) {
	out, err := runCapture("git", "-C", repoDir, "rev-list", "--count", spec)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("parsing rev-list output %q: %w", out, err)
	}
	return n, nil
}

func short7(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
