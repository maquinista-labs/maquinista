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

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/dashboard"
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

var (
	dashboardStartListen   string
	dashboardStartNoEmbed  string
	dashboardStartEmbedDir string
)

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
	dashboardStartCmd.Flags().StringVar(&dashboardStartListen, "listen", "", "host:port to bind (overrides MAQUINISTA_DASHBOARD_LISTEN and the default 127.0.0.1:8900)")
	dashboardStartCmd.Flags().StringVar(&dashboardStartNoEmbed, "no-embed", "", "path to a pre-built Next.js .next/standalone directory (skips the embedded extract step; CI uses this to avoid paying the tarball extract per test)")
	dashboardStartCmd.Flags().StringVar(&dashboardStartEmbedDir, "embed-dir", "", "override the extraction directory (default ~/.maquinista/dashboard/<version>)")
	dashboardLogsCmd.Flags().BoolVarP(&dashboardLogsFollow, "follow", "f", false, "follow the log file")
	dashboardCmd.AddCommand(dashboardStartCmd, dashboardStopCmd, dashboardStatusCmd, dashboardLogsCmd)
	rootCmd.AddCommand(dashboardCmd)
}

// resolveDashboardListen returns the configured listen address. Flag
// > env > default.
func resolveDashboardListen() string {
	if dashboardStartListen != "" {
		return dashboardStartListen
	}
	if v := os.Getenv("MAQUINISTA_DASHBOARD_LISTEN"); v != "" {
		return v
	}
	return "127.0.0.1:8900"
}

// resolveNodeBin returns the Node executable path. Env > default.
func resolveNodeBin() string {
	if v := os.Getenv("MAQUINISTA_DASHBOARD_NODE_BIN"); v != "" {
		return v
	}
	return "node"
}

// dashboardHealthcheckScript is the Phase 0 stub: a Node one-liner
// that responds with {"ok":true} on any path. Commit 1.6 replaces
// the spawn with the extracted Next.js standalone server, at which
// point this constant becomes dead code and is deleted.
const dashboardHealthcheckScript = `
const http = require('http');
const server = http.createServer((req, res) => {
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify({ ok: true, path: req.url, stub: true }));
});
const port = parseInt(process.env.PORT || '8900', 10);
const host = process.env.HOSTNAME || '127.0.0.1';
server.listen(port, host, () => {
  process.stdout.write("dashboard stub listening on " + host + ":" + port + "\n");
});
process.on('SIGTERM', () => server.close(() => process.exit(0)));
`

// parseListen splits "host:port" into its components. Returns the
// port only if the string is "port" form.
func parseListen(listen string) (host string, port string) {
	// Find the LAST colon so IPv6 ([::1]:8900) parses correctly.
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[:i], listen[i+1:]
		}
	}
	return "127.0.0.1", listen
}

// runDashboardStart spawns the Node child via dashboard.Supervisor
// and blocks until SIGTERM/SIGINT arrives or the restart budget is
// exhausted.
//
// Child-process resolution, in priority order:
//  1. --no-embed <dir>: run `node server.js` from <dir>. Used by
//     CI + local dev to avoid the extract step once a standalone
//     bundle is sitting in the tree.
//  2. Real embedded tarball: extract to the cache dir and run
//     `node <cache>/server.js`.
//  3. Placeholder tarball: fall back to the Phase 0 Node
//     healthcheck stub. This keeps `maquinista dashboard start`
//     working on a fresh clone before `make dashboard-web-package`
//     has run.
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

	listen := resolveDashboardListen()
	host, port := parseListen(listen)
	if port == "" {
		return fmt.Errorf("invalid --listen %q (expected host:port)", listen)
	}

	nodeBin := resolveNodeBin()
	if _, err := config.Load(); err != nil {
		// Config load is best-effort for Phase 0/1 — we can run
		// without Telegram config. Later phases (e.g. Phase 6
		// auth via Telegram magic link) will tighten this.
		_ = err
	}

	source, err := resolveDashboardChild(nodeBin)
	if err != nil {
		return err
	}

	logPath := dashboardLogFilePath()

	sup := dashboard.New(dashboard.Config{
		Bin:            source.Bin,
		Args:           source.Args,
		Env:            append([]string{"PORT=" + port, "HOSTNAME=" + host}, source.Env...),
		WorkDir:        source.WorkDir,
		LogPath:        logPath,
		MaxRestarts:    5,
		RestartWindow:  60 * time.Second,
		RestartBackoff: 500 * time.Millisecond,
	})

	if err := writeDashboardPIDFile(os.Getpid()); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}

	ctx, cancel := signal.NotifyContext(parentCtx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	fmt.Fprintf(os.Stdout, "dashboard: starting (listen=%s source=%s log=%s)\n", listen, source.Kind, logPath)

	runErr := sup.Run(ctx)

	removeDashboardPIDFile()

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "dashboard: supervisor error: %v\n", runErr)
		return runErr
	}

	fmt.Fprintln(os.Stdout, "dashboard: stopped")
	return nil
}

// dashboardChildSource describes how to spawn the dashboard's Node
// child. Kind is a short human-readable tag for logs and tests.
type dashboardChildSource struct {
	Kind    string
	Bin     string
	Args    []string
	Env     []string
	WorkDir string
}

// resolveDashboardChild picks between --no-embed, the real
// //go:embed tarball, and the Phase 0 healthcheck stub fallback.
func resolveDashboardChild(nodeBin string) (dashboardChildSource, error) {
	// Option 1: --no-embed
	if dashboardStartNoEmbed != "" {
		server := filepath.Join(dashboardStartNoEmbed, "server.js")
		if _, err := os.Stat(server); err != nil {
			return dashboardChildSource{}, fmt.Errorf("--no-embed %q: %w", dashboardStartNoEmbed, err)
		}
		return dashboardChildSource{
			Kind:    "no-embed:" + dashboardStartNoEmbed,
			Bin:     nodeBin,
			Args:    []string{server},
			WorkDir: dashboardStartNoEmbed,
		}, nil
	}

	// Option 3 (checked early): placeholder fallback. Gives the
	// operator a working healthz on a fresh clone.
	if dashboard.StandaloneIsPlaceholder() {
		fmt.Fprintln(os.Stderr, "dashboard: embedded bundle is the NOT_BUILT placeholder; running Phase 0 healthcheck stub (run `make dashboard-web-package` for the real Next server)")
		return dashboardChildSource{
			Kind: "stub",
			Bin:  nodeBin,
			Args: []string{"-e", dashboardHealthcheckScript},
		}, nil
	}

	// Option 2: extract the embedded tarball.
	dest := dashboardStartEmbedDir
	if dest == "" {
		version := dashboard.StandaloneSHA256()
		// Short prefix is enough to disambiguate across versions.
		dest = filepath.Join(resolveDashboardDir(), "dashboard", version[:16])
	}
	extracted, err := dashboard.ExtractStandalone(dest)
	if err != nil {
		return dashboardChildSource{}, fmt.Errorf("extract standalone: %w", err)
	}
	if extracted {
		fmt.Fprintf(os.Stdout, "dashboard: extracted embedded bundle to %s\n", dest)
	}
	server := filepath.Join(dest, "server.js")
	if _, err := os.Stat(server); err != nil {
		return dashboardChildSource{}, fmt.Errorf("extracted bundle missing server.js at %s: %w", server, err)
	}
	return dashboardChildSource{
		Kind:    "embedded:" + dest,
		Bin:     nodeBin,
		Args:    []string{server},
		WorkDir: dest,
	}, nil
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

// runDashboardLogs prints the dashboard log to out. If follow is
// true, tails the file until ctx is cancelled — new content is
// streamed as it's appended.
//
// The tailing implementation is a simple poll loop rather than
// fsnotify: the log file only grows (never rotates mid-run) and
// polling every 100 ms is negligible. fsnotify would be nice but
// introduces a cross-platform dependency for marginal benefit.
func runDashboardLogs(ctx context.Context, out io.Writer, follow bool) error {
	path := dashboardLogFilePath()

	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		if !follow {
			fmt.Fprintf(out, "dashboard: no log file at %s (start the dashboard first)\n", path)
			return nil
		}
		// --follow: wait for the file to appear.
		fmt.Fprintf(out, "dashboard: waiting for %s to appear\n", path)
		f, err = waitForDashboardLog(ctx, path)
		if err != nil {
			return err
		}
	}
	defer f.Close()

	// Dump existing content.
	if _, err := io.Copy(out, f); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if !follow {
		return nil
	}

	// From here on: poll for new content until ctx is cancelled.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		// Check whether the file was truncated or recreated (e.g.
		// supervisor rotated it). If the file's current size is
		// less than our current offset, re-open from the top.
		cur, err := f.Seek(0, 1) // SEEK_CUR
		if err != nil {
			return fmt.Errorf("seek: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			// File was removed — keep waiting rather than erroring.
			continue
		}
		if info.Size() < cur {
			f.Close()
			f, err = os.Open(path)
			if err != nil {
				return fmt.Errorf("reopen %s: %w", path, err)
			}
		}

		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
		}
	}
}

// waitForDashboardLog polls for the log file to appear. Returns
// ctx.Err() if the context fires before the file exists.
func waitForDashboardLog(ctx context.Context, path string) (*os.File, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
