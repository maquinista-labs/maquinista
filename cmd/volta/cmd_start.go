package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/otaviocarvalho/volta/internal/bot"
	"github.com/otaviocarvalho/volta/internal/config"
	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/otaviocarvalho/volta/internal/listener"
	"github.com/otaviocarvalho/volta/internal/monitor"
	"github.com/otaviocarvalho/volta/internal/orchestrator"
	"github.com/otaviocarvalho/volta/internal/queue"
	"github.com/otaviocarvalho/volta/internal/runner"
	"github.com/otaviocarvalho/volta/internal/state"
	"github.com/otaviocarvalho/volta/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	// start --orchestrate flags
	startOrchestrate   bool
	startOrchProject   string
	startOrchMaxAgents int
	startOrchRunner    string
	startOrchWorktrees bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Telegram bot daemon",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if cfgPath != "" {
			return nil
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStart()
	},
}

func init() {
	startCmd.Flags().StringVar(&cfgPath, "env", "", "path to .env config file")
	startCmd.Flags().BoolVar(&startOrchestrate, "orchestrate", false, "run orchestrator alongside bot")
	startCmd.Flags().StringVar(&startOrchProject, "orchestrate-project", "", "project for orchestrator")
	startCmd.Flags().IntVar(&startOrchMaxAgents, "orchestrate-max-agents", 3, "max agents for orchestrator")
	startCmd.Flags().StringVar(&startOrchRunner, "orchestrate-runner", "claude", "runner for orchestrator")
	startCmd.Flags().BoolVar(&startOrchWorktrees, "orchestrate-worktrees", false, "use worktrees for orchestrator agents")
}

func pidFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/volta.pid"
	}
	dir := filepath.Join(home, ".volta")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "volta.pid")
}

func writePIDFile() error {
	return os.WriteFile(pidFilePath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func removePIDFile() {
	_ = os.Remove(pidFilePath())
}

// readPIDFile returns the PID from the PID file. Returns 0 if the file doesn't exist.
func readPIDFile() (int, error) {
	data, err := os.ReadFile(pidFilePath())
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

// processAlive checks if a process with the given PID is running.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if process exists without actually sending a signal.
	return proc.Signal(syscall.Signal(0)) == nil
}

func runStart() error {
	// Check for existing instance.
	pid, err := readPIDFile()
	if err != nil {
		return fmt.Errorf("reading PID file: %w", err)
	}
	if pid != 0 {
		if processAlive(pid) {
			return fmt.Errorf("volta is already running (PID %d), use 'volta stop' first", pid)
		}
		log.Printf("Cleaning up stale PID file (PID %d is dead)", pid)
		removePIDFile()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Write PID file.
	if err := writePIDFile(); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}

	b, err := bot.New(cfg)
	if err != nil {
		removePIDFile()
		return fmt.Errorf("creating bot: %w", err)
	}

	msPath := filepath.Join(cfg.VoltaDir, "monitor_state.json")
	ms, err := state.LoadMonitorState(msPath)
	if err != nil {
		log.Printf("Warning: loading monitor state: %v (starting fresh)", err)
		ms = state.NewMonitorState()
	}
	b.SetMonitorState(ms)

	liveBindings := b.ReconcileState()
	log.Printf("Startup: %d live bindings recovered", liveBindings)

	q := queue.New(b.API())
	b.SetQueue(q)

	mon := monitor.New(cfg, b.State(), ms, q)
	mon.PlanHandler = b.HandlePlanFromMonitor

	sp := bot.NewStatusPoller(b, q, mon)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Remove PID file on shutdown.
	go func() {
		<-ctx.Done()
		removePIDFile()
	}()

	go mon.Run(ctx)
	go sp.Run(ctx)

	// Start orchestrator if requested.
	if startOrchestrate {
		if pool == nil && cfg.DatabaseURL != "" {
			var dbErr error
			pool, dbErr = db.Connect(cfg.DatabaseURL)
			if dbErr != nil {
				log.Printf("Warning: failed to connect DB for orchestrator: %v", dbErr)
			}
		}
		if pool == nil {
			log.Println("Warning: --orchestrate requires DATABASE_URL for DB pool")
			startOrchestrate = false
		}
	}
	if startOrchestrate {
		orchProject := startOrchProject
		if orchProject == "" {
			orchProject = os.Getenv("VOLTA_PROJECT")
		}
		if orchProject == "" {
			log.Println("Warning: --orchestrate requires --orchestrate-project or VOLTA_PROJECT")
		} else {
			r, rErr := runner.Get(startOrchRunner)
			if rErr != nil {
				log.Printf("Warning: unknown orchestrator runner %q: %v", startOrchRunner, rErr)
			} else {
				claudeMD, mdErr := findClaudeMD()
				if mdErr != nil {
					log.Printf("Warning: cannot find CLAUDE.md for orchestrator: %v", mdErr)
				} else {
					if err := tmux.EnsureSession(cfg.TmuxSessionName); err != nil {
						log.Printf("Warning: ensuring tmux session for orchestrator: %v", err)
					}

					el := listener.New(cfg.DatabaseURL)
					go el.Start(ctx)
					notifyCh := orchestrator.NotifyBridge(ctx, el.TaskEvents)

					orchCfg := orchestrator.Config{
						Pool:         pool,
						Runner:       r,
						TmuxSession:  cfg.TmuxSessionName,
						ProjectID:    orchProject,
						MaxAgents:    startOrchMaxAgents,
						PollInterval: 10 * time.Second,
						UseWorktrees: startOrchWorktrees,
						ClaudeMDPath: claudeMD,
						DatabaseURL:  cfg.DatabaseURL,
						NotifyCh:     notifyCh,
						NotifyFunc: func(message string) {
							log.Printf("Orchestrator: %s", message)
						},
					}

					go func() {
						if err := orchestrator.Run(ctx, orchCfg); err != nil {
							log.Printf("Orchestrator error: %v", err)
						}
					}()
					log.Printf("Orchestrator started: project=%s maxAgents=%d runner=%s",
						orchProject, startOrchMaxAgents, startOrchRunner)
				}
			}
		}
	}

	err = b.Run(ctx)

	log.Println("Saving state...")
	if saveErr := ms.ForceSave(msPath); saveErr != nil {
		log.Printf("Error saving monitor state: %v", saveErr)
	}

	return err
}
