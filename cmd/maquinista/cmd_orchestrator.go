package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/daemonize"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

// Step D.3 of plans/active/detached-processes.md — `maquinista
// orchestrator start|stop|status|logs` lives here. The actual
// orchestrator supervisor body (bot + monitor + mailbox + optional
// orchestrator engine) still lives in cmd_start.go's
// runOrchestratorSupervised, called from the daemonize.Run
// foreground path.

// maquinistaDir resolves ~/.maquinista (falling back to /tmp if
// $HOME is unset). Both the orchestrator PID / log paths and the
// legacy maquinista.pid live under it.
func maquinistaDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/maquinista"
	}
	dir := filepath.Join(home, ".maquinista")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// orchestratorPIDFilePath returns the canonical PID file path. The
// pre-D.3 location was ~/.maquinista/maquinista.pid; see
// legacyMaquinistaPIDPath for the migration helper.
func orchestratorPIDFilePath() string {
	return filepath.Join(maquinistaDir(), "orchestrator.pid")
}

func orchestratorLogFilePath() string {
	dir := filepath.Join(maquinistaDir(), "logs")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "orchestrator.log")
}

// legacyMaquinistaPIDPath is the pre-D.3 PID-file location. Stop
// checks it for one release cycle so a pre-upgrade daemon is still
// killable. Delete after the next release.
func legacyMaquinistaPIDPath() string {
	return filepath.Join(maquinistaDir(), "maquinista.pid")
}

func orchestratorSpec() daemonize.Spec {
	return daemonize.Spec{
		Name:       "orchestrator",
		LogPath:    orchestratorLogFilePath(),
		PIDPath:    orchestratorPIDFilePath(),
		Foreground: orchestratorStartForeground,
	}
}

var orchestratorCmd = &cobra.Command{
	Use:   "orchestrator",
	Short: "Control the maquinista orchestrator daemon (bot + dispatcher + mailbox)",
}

var (
	orchestratorStartForeground bool
	orchestratorLogsFollow      bool
)

var orchestratorStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the orchestrator daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOrchestratorStart(cmd.Context())
	},
}

var orchestratorStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the orchestrator daemon (also kills tmux session + DB agents)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOrchestratorStop()
	},
}

var orchestratorStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report the orchestrator's running status",
	RunE: func(cmd *cobra.Command, args []string) error {
		pid, alive, err := daemonize.Status(orchestratorSpec())
		if err != nil {
			return err
		}
		if !alive {
			fmt.Fprintln(cmd.OutOrStdout(), "orchestrator: not running")
			os.Exit(1)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "orchestrator: running (PID %d)\n", pid)
		return nil
	},
}

var orchestratorLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the orchestrator log file",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemonize.TailLogs(cmd.Context(), orchestratorSpec(), orchestratorLogsFollow, cmd.OutOrStdout())
	},
}

func init() {
	orchestratorStartCmd.Flags().StringVar(&cfgPath, "env", "", "path to .env config file")
	orchestratorStartCmd.Flags().StringVar(&startRunner, "runner", "", "default agent runner (claude, openclaude, opencode)")
	orchestratorStartCmd.Flags().StringVar(&startAgentCWD, "agent-cwd", "", "working dir inherited by newly-spawned topic agents (overrides cfg.DefaultAgentCWD; defaults to $PWD)")
	orchestratorStartCmd.Flags().BoolVar(&startOrchestrate, "orchestrate", false, "run orchestrator engine alongside bot")
	orchestratorStartCmd.Flags().StringVar(&startOrchProject, "orchestrate-project", "", "project for orchestrator engine")
	orchestratorStartCmd.Flags().IntVar(&startOrchMaxAgents, "orchestrate-max-agents", 3, "max agents for orchestrator engine")
	orchestratorStartCmd.Flags().StringVar(&startOrchRunner, "orchestrate-runner", "claude", "runner for orchestrator engine")
	orchestratorStartCmd.Flags().BoolVar(&startOrchWorktrees, "orchestrate-worktrees", false, "use worktrees for orchestrator engine agents")
	orchestratorStartCmd.Flags().BoolVarP(&orchestratorStartForeground, "foreground", "F", false, "run in the current terminal (default: detach and return immediately)")

	orchestratorLogsCmd.Flags().BoolVarP(&orchestratorLogsFollow, "follow", "f", false, "follow the log file")

	orchestratorCmd.AddCommand(orchestratorStartCmd, orchestratorStopCmd, orchestratorStatusCmd, orchestratorLogsCmd)
	rootCmd.AddCommand(orchestratorCmd)
}

// runOrchestratorStart wraps the supervisor body in daemonize.Run.
// The foreground branch runs runOrchestratorSupervised in-process;
// the detach branch re-execs the binary with --foreground appended
// so the child lands in the same command path.
func runOrchestratorStart(parentCtx context.Context) error {
	return daemonize.Run(parentCtx, orchestratorSpec(), func(ctx context.Context) error {
		return runOrchestratorSupervised(ctx)
	})
}

// runOrchestratorStop signals the orchestrator daemon and performs
// the tmux + DB cleanup that used to live under the top-level `stop`
// command. Owning the cleanup here matches the lifecycle: a dead
// daemon should leave no tmux sessions or claimed DB agents behind.
func runOrchestratorStop() error {
	spec := orchestratorSpec()

	// Phase 1: read PID (pre-Stop, so we can report "was running").
	pid, alive, err := daemonize.Status(spec)
	if err != nil {
		log.Printf("Warning: reading PID file: %v", err)
	}

	// Phase 2: also honour the legacy maquinista.pid for one release
	// cycle. Pre-D.3 daemons wrote there.
	legacyPID := readLegacyMaquinistaPID()

	// Phase 3: signal the canonical daemon if alive.
	if err := daemonize.Stop(spec, 10*time.Second); err != nil {
		log.Printf("Warning: stopping orchestrator: %v", err)
	}

	// Phase 4: kill a pre-D.3 leftover if present.
	if legacyPID > 0 && processStillAlive(legacyPID) {
		log.Printf("Legacy maquinista.pid points to live PID %d — sending SIGTERM", legacyPID)
		_ = syscall.Kill(legacyPID, syscall.SIGTERM)
		waitForExit(legacyPID, 10*time.Second)
		if processStillAlive(legacyPID) {
			_ = syscall.Kill(legacyPID, syscall.SIGKILL)
		}
	}
	if legacyPID != 0 {
		_ = os.Remove(legacyMaquinistaPIDPath())
	}

	// Phase 5: tmux + DB agent cleanup (best-effort).
	cfg, cfgErr := config.Load()
	sessionName := "maquinista"
	if cfgErr == nil {
		sessionName = cfg.TmuxSessionName
	}
	if tmux.SessionExists(sessionName) {
		log.Printf("Killing tmux session %q...", sessionName)
		if err := tmux.KillSession(sessionName); err != nil {
			log.Printf("Warning: killing tmux session: %v", err)
		}
	}
	dbURL := os.Getenv("DATABASE_URL")
	if cfgErr == nil && cfg.DatabaseURL != "" {
		dbURL = cfg.DatabaseURL
	}
	if dbURL != "" {
		if cleanPool, err := db.Connect(dbURL); err != nil {
			log.Printf("Warning: connecting to DB for agent cleanup: %v", err)
		} else {
			if err := agent.KillAll(cleanPool, sessionName); err != nil {
				log.Printf("Warning: killing DB agents: %v", err)
			}
			cleanPool.Close()
		}
	}

	if alive {
		log.Printf("Orchestrator stopped (PID %d)", pid)
	} else if legacyPID != 0 {
		log.Printf("Legacy orchestrator stopped (PID %d)", legacyPID)
	} else {
		log.Println("Orchestrator is not running.")
	}
	return nil
}

// readLegacyMaquinistaPID parses ~/.maquinista/maquinista.pid if
// present. Returns 0 on missing / malformed — the migration helper
// is best-effort.
func readLegacyMaquinistaPID() int {
	data, err := os.ReadFile(legacyMaquinistaPIDPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// processStillAlive is a local alias for the daemonize private
// probe — reimplemented here because it's unexported upstream. A
// tiny duplicate is cheaper than plumbing a new export.
func processStillAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func waitForExit(pid int, grace time.Duration) {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processStillAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

