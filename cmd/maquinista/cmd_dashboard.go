package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/daemonize"
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
	// daemonize writes with a trailing newline; earlier versions
	// didn't. Trim whitespace so either format parses.
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
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
	dashboardStartListen     string
	dashboardStartNoEmbed    string
	dashboardStartEmbedDir   string
	dashboardStartForeground bool
)

// dashboardSpec returns the daemonize.Spec for the dashboard daemon.
// Path resolution goes through dashboardPIDFilePath / dashboardLogFilePath
// so tests can override the state directory via SetDashboardDir.
func dashboardSpec() daemonize.Spec {
	return daemonize.Spec{
		Name:       "dashboard",
		LogPath:    dashboardLogFilePath(),
		PIDPath:    dashboardPIDFilePath(),
		Foreground: dashboardStartForeground,
		// Pin the re-exec'd child to "dashboard start" so the top-
		// level `maquinista start` bootstrap (which calls
		// runDashboardStart while os.Args is ["maquinista","start"])
		// can't accidentally re-exec the child into the wrong
		// command path.
		ChildArgs: []string{"dashboard", "start"},
	}
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
	dashboardStartCmd.Flags().StringVar(&dashboardStartListen, "listen", "", "host:port to bind (overrides MAQUINISTA_DASHBOARD_LISTEN and the default 127.0.0.1:8900)")
	dashboardStartCmd.Flags().StringVar(&dashboardStartNoEmbed, "no-embed", "", "path to a pre-built Next.js .next/standalone directory (skips the embedded extract step; CI uses this to avoid paying the tarball extract per test)")
	dashboardStartCmd.Flags().StringVar(&dashboardStartEmbedDir, "embed-dir", "", "override the extraction directory (default ~/.maquinista/dashboard/<version>)")
	dashboardStartCmd.Flags().BoolVarP(&dashboardStartForeground, "foreground", "F", false, "run in the current terminal (default: detach and return immediately)")
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

// runDashboardStart drives the dashboard lifecycle. Default behavior
// is to detach: re-exec with --foreground appended, redirect the
// child's stdio to dashboard.log, print the child PID, and return.
// With --foreground, we supervise the Node child inline and block
// until ctx is cancelled, SIGTERM/SIGINT arrives, or the restart
// budget is exhausted.
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
	// Cheap pre-flight validations run in both paths so bad input
	// surfaces in the user's terminal rather than in a log file the
	// detached child is the only one writing.
	listen := resolveDashboardListen()
	if _, port := parseListen(listen); port == "" {
		return fmt.Errorf("invalid --listen %q (expected host:port)", listen)
	}
	if dashboardStartNoEmbed != "" {
		server := filepath.Join(dashboardStartNoEmbed, "server.js")
		if _, err := os.Stat(server); err != nil {
			return fmt.Errorf("--no-embed %q: %w", dashboardStartNoEmbed, err)
		}
	}

	return daemonize.Run(parentCtx, dashboardSpec(), func(ctx context.Context) error {
		return superviseDashboard(ctx, listen)
	})
}

// superviseDashboard is the dashboard's foreground worker: resolve
// the Node child, spin up dashboard.Supervisor, and block on it.
// Invoked from daemonize.Run's foreground branch (either because
// the operator passed --foreground, or because we are the re-exec'd
// detached child).
func superviseDashboard(ctx context.Context, listen string) error {
	host, port := parseListen(listen)
	nodeBin := resolveNodeBin()

	// Resolve dashboard config so we can explicitly forward
	// MAQUINISTA_DASHBOARD_AUTH to the Node child. The Go default
	// ("password" when unset) differs from Next.js's default ("none"),
	// so we must pin the resolved value rather than relying on
	// inheritance. Config load is best-effort: Telegram / ALLOWED_USERS
	// may not be set in some envs; fall back to the safe default.
	authMode := "password"
	if cfg, cfgErr := config.Load(); cfgErr == nil {
		authMode = cfg.Dashboard.AuthMode
		if cfg.Dashboard.AutoTunnel && authMode == "none" {
			fmt.Fprintln(os.Stderr,
				"WARNING: MAQUINISTA_DASHBOARD_AUTO_TUNNEL=1 but MAQUINISTA_DASHBOARD_AUTH=none. "+
					"The dashboard is publicly accessible without authentication. "+
					"Set MAQUINISTA_DASHBOARD_AUTH=password to require login.")
		}
	}

	source, err := resolveDashboardChild(nodeBin)
	if err != nil {
		return err
	}

	logPath := dashboardLogFilePath()
	sup := dashboard.New(dashboard.Config{
		Bin:  source.Bin,
		Args: source.Args,
		Env: append([]string{
			"PORT=" + port,
			"HOSTNAME=" + host,
			// Explicitly forward the resolved auth mode so the Next.js
			// middleware always sees the Go-side default ("password")
			// even when the operator hasn't set the env var explicitly.
			"MAQUINISTA_DASHBOARD_AUTH=" + authMode,
		}, source.Env...),
		WorkDir:        source.WorkDir,
		LogPath:        logPath,
		ListenPort:     port,
		MaxRestarts:    5,
		RestartWindow:  60 * time.Second,
		RestartBackoff: 500 * time.Millisecond,
	})

	fmt.Fprintf(os.Stdout, "dashboard: starting (listen=%s source=%s log=%s)\n", listen, source.Kind, logPath)

	runErr := sup.Run(ctx)
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

// runDashboardStop delegates to daemonize.Stop. Output mirrors the
// previous inline behaviour so operator tooling / tests see the same
// strings.
func runDashboardStop() error {
	spec := dashboardSpec()
	pid, alive, err := daemonize.Status(spec)
	if err != nil {
		return fmt.Errorf("reading PID file: %w", err)
	}
	if pid == 0 && !alive {
		// Either no PID file or a stale one. Stop will clean up the
		// stale file if present.
		_ = daemonize.Stop(spec, 10*time.Second)
		// Distinguish "no file" from "stale" in operator output.
		if _, statErr := os.Stat(spec.PIDPath); statErr == nil {
			// Shouldn't happen after Stop, but be safe.
			fmt.Fprintln(os.Stdout, "dashboard: not running")
			return nil
		}
		fmt.Fprintln(os.Stdout, "dashboard: not running")
		return nil
	}
	if err := daemonize.Stop(spec, 10*time.Second); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "dashboard: stopped (PID %d)\n", pid)
	return nil
}

// runDashboardStatus returns (running, pid, err). Caller decides how
// to render / exit-code.
func runDashboardStatus() (bool, int, error) {
	pid, alive, err := daemonize.Status(dashboardSpec())
	if err != nil {
		return false, 0, fmt.Errorf("reading PID file: %w", err)
	}
	return alive, pid, nil
}

// runDashboardLogs prints the dashboard log to out. If follow is
// true, tails the file until ctx is cancelled.
func runDashboardLogs(ctx context.Context, out io.Writer, follow bool) error {
	return daemonize.TailLogs(ctx, dashboardSpec(), follow, out)
}
