package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Phase 0 of plans/active/dashboard.md — `maquinista dashboard
// start|stop|status|logs` scaffolding. This commit (0.1) wires the
// cobra subcommands and the PID-file helpers; the `start` subcommand
// is a no-child stub that blocks on SIGTERM so integration tests can
// exercise the lifecycle. Commit 0.3 replaces the stub with a Node
// healthcheck child; Commit 1.6 swaps that for the embedded Next.js
// standalone server.

// dashboardDir is overridable by tests via SetDashboardDir so the PID
// file + log file don't land in the user's real ~/.maquinista.
var dashboardDir string

// SetDashboardDir overrides the dashboard state directory. Tests call
// this with a t.TempDir() path; production code leaves it unset so
// the resolver falls through to ~/.maquinista.
func SetDashboardDir(dir string) { dashboardDir = dir }

func resolveDashboardDir() string {
	if dashboardDir != "" {
		return dashboardDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/maquinista"
	}
	return filepath.Join(home, ".maquinista")
}

func dashboardPIDFilePath() string {
	dir := resolveDashboardDir()
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "dashboard.pid")
}

func dashboardLogFilePath() string {
	dir := filepath.Join(resolveDashboardDir(), "logs")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "dashboard.log")
}

func writeDashboardPIDFile(pid int) error {
	return os.WriteFile(dashboardPIDFilePath(), []byte(strconv.Itoa(pid)), 0o644)
}

func removeDashboardPIDFile() {
	_ = os.Remove(dashboardPIDFilePath())
}

// readDashboardPIDFile returns the PID from the PID file. Returns 0
// (and nil error) if the file doesn't exist; returns an error if the
// file exists but is malformed.
func readDashboardPIDFile() (int, error) {
	data, err := os.ReadFile(dashboardPIDFilePath())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("invalid PID file: %w", err)
	}
	return pid, nil
}

// dashboardProcessAlive reports whether a process with the given PID
// is currently running. Uses signal 0 (POSIX probe) which doesn't
// actually signal the process.
func dashboardProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Control the maquinista dashboard (Next.js, supervised)",
	Long: `Start, stop, inspect, or tail the dashboard process.

The dashboard is a Next.js application supervised by this CLI. See
plans/active/dashboard.md for the full architecture.`,
}

var dashboardStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDashboardStart(cmd.Context())
	},
}

var dashboardStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDashboardStop()
	},
}

var dashboardStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report the dashboard's running status",
	RunE: func(cmd *cobra.Command, args []string) error {
		running, pid, err := runDashboardStatus()
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintln(cmd.OutOrStdout(), "dashboard: not running")
			os.Exit(1)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "dashboard: running (PID %d)\n", pid)
		return nil
	},
}

var (
	dashboardLogsFollow bool
)

var dashboardLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the dashboard log file",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDashboardLogs(cmd.Context(), cmd.OutOrStdout(), dashboardLogsFollow)
	},
}

func init() {
	dashboardLogsCmd.Flags().BoolVarP(&dashboardLogsFollow, "follow", "f", false, "follow the log file")
	dashboardCmd.AddCommand(dashboardStartCmd, dashboardStopCmd, dashboardStatusCmd, dashboardLogsCmd)
	rootCmd.AddCommand(dashboardCmd)
}

// runDashboardStart is the Phase 0 Commit 0.1 skeleton. It:
//  1. Refuses to start if a live PID is already on disk.
//  2. Cleans up a stale PID file if the recorded process is dead.
//  3. Writes the current process's PID to the file and blocks on
//     SIGTERM / SIGINT. Commit 0.3 replaces the block with a Node
//     child spawn + supervisor.Wait().
func runDashboardStart(parentCtx context.Context) error {
	existing, err := readDashboardPIDFile()
	if err != nil {
		return fmt.Errorf("reading PID file: %w", err)
	}
	if existing != 0 {
		if dashboardProcessAlive(existing) {
			return fmt.Errorf("dashboard is already running (PID %d); use 'maquinista dashboard stop' first", existing)
		}
		removeDashboardPIDFile()
	}

	if err := writeDashboardPIDFile(os.Getpid()); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}

	ctx, cancel := signal.NotifyContext(parentCtx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	fmt.Fprintln(os.Stdout, "dashboard: started (stub — no server yet)")
	fmt.Fprintf(os.Stdout, "dashboard: PID %d, waiting for SIGTERM/SIGINT\n", os.Getpid())

	<-ctx.Done()

	removeDashboardPIDFile()
	fmt.Fprintln(os.Stdout, "dashboard: stopped")
	return nil
}

// runDashboardStop reads the PID file and terminates the recorded
// process with SIGTERM, escalating to SIGKILL after a 10 s grace.
// Tolerates missing / stale PID files (returns nil with a message).
func runDashboardStop() error {
	pid, err := readDashboardPIDFile()
	if err != nil {
		return fmt.Errorf("reading PID file: %w", err)
	}
	if pid == 0 {
		fmt.Fprintln(os.Stdout, "dashboard: not running")
		return nil
	}
	if !dashboardProcessAlive(pid) {
		removeDashboardPIDFile()
		fmt.Fprintf(os.Stdout, "dashboard: stale PID %d cleaned up\n", pid)
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM to %d: %w", pid, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !dashboardProcessAlive(pid) {
			removeDashboardPIDFile()
			fmt.Fprintf(os.Stdout, "dashboard: stopped (PID %d)\n", pid)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = proc.Signal(syscall.SIGKILL)
	// Give SIGKILL a beat to take effect before removing the PID file
	// so a racing status check reports honest state.
	for i := 0; i < 20 && dashboardProcessAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	removeDashboardPIDFile()
	fmt.Fprintf(os.Stdout, "dashboard: killed (PID %d, did not respond to SIGTERM)\n", pid)
	return nil
}

// runDashboardStatus returns (running, pid, err). Caller decides how
// to render / exit-code.
func runDashboardStatus() (bool, int, error) {
	pid, err := readDashboardPIDFile()
	if err != nil {
		return false, 0, fmt.Errorf("reading PID file: %w", err)
	}
	if pid == 0 || !dashboardProcessAlive(pid) {
		return false, 0, nil
	}
	return true, pid, nil
}

// runDashboardLogs tails the dashboard log. Phase 0 Commit 0.5
// expands this with --follow; Commit 0.1 ships a read-once view that
// prints an explanatory message if the log file doesn't exist yet.
func runDashboardLogs(ctx context.Context, out io.Writer, follow bool) error {
	path := dashboardLogFilePath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Fprintf(out, "dashboard: no log file at %s (start the dashboard first)\n", path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := out.Write(data); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	// Commit 0.5 implements --follow. Skeleton returns after a
	// single read so the flag is callable today.
	_ = ctx
	return nil
}
