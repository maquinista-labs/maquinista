// Package daemonize is a reusable helper for running long-lived
// commands as detached daemons with a PID file + log file. See
// plans/active/detached-processes.md for the motivation.
//
// Production callers pass a Spec and a work function to Run. If the
// spec's Foreground is false, Run re-execs the current binary with
// --foreground appended, detaches the child via Setsid, redirects
// stdout+stderr to LogPath, prints the child's PID to stdout, and
// returns nil immediately. The caller's shell is released.
//
// If Foreground is true, Run executes work in the current process
// with SIGINT/SIGTERM wired to ctx cancel, writes the PID file on
// entry, and removes it on exit. This is also the path the re-exec'd
// child takes.
package daemonize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Spec configures one daemon instance. A Spec is read at Run / Stop /
// Status time — mutating it after those calls has no effect.
type Spec struct {
	// Name is a short human-readable tag used in error messages and
	// the "<name>: started (PID ...)" line printed after detach.
	// Example: "orchestrator", "dashboard".
	Name string
	// LogPath is the file the detached child's stdout + stderr are
	// redirected to (O_APPEND | O_CREATE, 0o644). Parent directories
	// are created if missing. Only used when detaching; foreground
	// mode leaves stdio on the terminal.
	LogPath string
	// PIDPath is the PID file. Parent directories are created if
	// missing. The child writes its own PID at startup and removes
	// the file on clean shutdown.
	PIDPath string
	// Foreground selects the in-process path. False means "detach via
	// re-exec" (user-facing default); true means "run work here".
	// Cobra commands wire this from a --foreground / -F flag; the
	// re-exec'd child always sees Foreground=true because detach
	// appends --foreground to argv.
	Foreground bool
}

// buildDetachCmd produces the exec.Cmd used by the detach path. It's
// a package-level var so tests can replace it with a helper that
// spawns the test binary in a controlled child mode.
var buildDetachCmd = func() (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving executable: %w", err)
	}
	childArgs := append([]string{}, os.Args[1:]...)
	if !hasForegroundFlag(childArgs) {
		childArgs = append(childArgs, "--foreground")
	}
	return exec.Command(exe, childArgs...), nil
}

// Run executes work as the daemon's main loop. See the package doc
// for the two branches (detach vs foreground).
func Run(spec Spec, work func(ctx context.Context) error) error {
	if spec.Foreground {
		return runForeground(spec, work)
	}
	return detach(spec)
}

// Stop reads the PID file and terminates the recorded process with
// SIGTERM. If the process is still alive after grace, escalates to
// SIGKILL. Removes the PID file on success. Returns nil (and does
// nothing) if the PID file is missing or the recorded PID is not
// alive — the daemon is already stopped.
//
// If grace <= 0, 10 seconds is used.
func Stop(spec Spec, grace time.Duration) error {
	pid, err := readPID(spec.PIDPath)
	if err != nil {
		return fmt.Errorf("%s: reading PID file: %w", spec.Name, err)
	}
	if pid == 0 {
		return nil
	}
	if !processAlive(pid) {
		_ = os.Remove(spec.PIDPath)
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("%s: finding process %d: %w", spec.Name, pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("%s: sending SIGTERM to %d: %w", spec.Name, pid, err)
	}
	if grace <= 0 {
		grace = 10 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			_ = os.Remove(spec.PIDPath)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	// Give SIGKILL a beat to take effect so a racing Status call sees
	// honest state before we remove the file.
	for i := 0; i < 20 && processAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(spec.PIDPath)
	return nil
}

// Status reports whether the recorded PID is currently alive. A
// missing or stale PID file reports (0, false, nil). A malformed
// PID file bubbles an error.
func Status(spec Spec) (pid int, alive bool, err error) {
	p, err := readPID(spec.PIDPath)
	if err != nil {
		return 0, false, err
	}
	if p == 0 || !processAlive(p) {
		return 0, false, nil
	}
	return p, true, nil
}

// ----- internal -------------------------------------------------------------

func runForeground(spec Spec, work func(ctx context.Context) error) error {
	existing, err := readPID(spec.PIDPath)
	if err != nil {
		return fmt.Errorf("%s: reading PID file: %w", spec.Name, err)
	}
	if existing != 0 {
		if processAlive(existing) {
			return fmt.Errorf("%s is already running (PID %d)", spec.Name, existing)
		}
		_ = os.Remove(spec.PIDPath)
	}
	if err := ensureDir(filepath.Dir(spec.PIDPath)); err != nil {
		return fmt.Errorf("%s: PID dir: %w", spec.Name, err)
	}
	if err := writePID(spec.PIDPath, os.Getpid()); err != nil {
		return fmt.Errorf("%s: writing PID file: %w", spec.Name, err)
	}
	defer func() { _ = os.Remove(spec.PIDPath) }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return work(ctx)
}

func detach(spec Spec) error {
	existing, err := readPID(spec.PIDPath)
	if err != nil {
		return fmt.Errorf("%s: reading PID file: %w", spec.Name, err)
	}
	if existing != 0 {
		if processAlive(existing) {
			return fmt.Errorf("%s is already running (PID %d)", spec.Name, existing)
		}
		_ = os.Remove(spec.PIDPath)
	}
	if spec.LogPath == "" {
		return fmt.Errorf("%s: LogPath is required for detach (pass --foreground to opt out)", spec.Name)
	}
	if err := ensureDir(filepath.Dir(spec.LogPath)); err != nil {
		return fmt.Errorf("%s: log dir: %w", spec.Name, err)
	}
	if err := ensureDir(filepath.Dir(spec.PIDPath)); err != nil {
		return fmt.Errorf("%s: PID dir: %w", spec.Name, err)
	}
	logFile, err := os.OpenFile(spec.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("%s: opening %s: %w", spec.Name, spec.LogPath, err)
	}
	defer logFile.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%s: opening %s: %w", spec.Name, os.DevNull, err)
	}
	defer devnull.Close()

	cmd, err := buildDetachCmd()
	if err != nil {
		return err
	}
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: starting detached: %w", spec.Name, err)
	}
	pid := cmd.Process.Pid
	// Release so Go stops tracking the child and doesn't Wait on it
	// when the parent exits. The child is the session leader via
	// Setsid and survives the parent on its own.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("%s: releasing process: %w", spec.Name, err)
	}
	fmt.Fprintf(os.Stdout, "%s: started (PID %d, log %s)\n", spec.Name, pid, spec.LogPath)
	return nil
}

// hasForegroundFlag reports whether argv already contains a
// --foreground / -F style flag. Covers `--foreground`,
// `--foreground=true`, `-F`, `-F=true` — the common forms cobra
// accepts for a bool flag.
func hasForegroundFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--foreground" || a == "-F" {
			return true
		}
		if strings.HasPrefix(a, "--foreground=") || strings.HasPrefix(a, "-F=") {
			return true
		}
	}
	return false
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, fmt.Errorf("invalid PID file %s: empty", path)
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid PID file %s: %w", path, err)
	}
	return pid, nil
}

func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func ensureDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
