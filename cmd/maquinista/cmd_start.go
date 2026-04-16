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

	"github.com/maquinista-labs/maquinista/hook"
	"github.com/maquinista-labs/maquinista/internal/bot"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/jobreg"
	"github.com/maquinista-labs/maquinista/internal/listener"
	"github.com/maquinista-labs/maquinista/internal/monitor"
	"github.com/maquinista-labs/maquinista/internal/orchestrator"
	"github.com/maquinista-labs/maquinista/internal/queue"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/state"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	// start --runner flag (default runner for all agents)
	startRunner string
	// start --agent / --agent-cwd flags (default agent auto-spawn)
	startAgentCWD string
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
	startCmd.Flags().StringVar(&startRunner, "runner", "", "default agent runner (claude, openclaude, opencode)")
	startCmd.Flags().StringVar(&startAgentCWD, "agent-cwd", "", "working dir inherited by newly-spawned topic agents (overrides cfg.DefaultAgentCWD; defaults to $PWD)")
	startCmd.Flags().BoolVar(&startOrchestrate, "orchestrate", false, "run orchestrator alongside bot")
	startCmd.Flags().StringVar(&startOrchProject, "orchestrate-project", "", "project for orchestrator")
	startCmd.Flags().IntVar(&startOrchMaxAgents, "orchestrate-max-agents", 3, "max agents for orchestrator")
	startCmd.Flags().StringVar(&startOrchRunner, "orchestrate-runner", "claude", "runner for orchestrator")
	startCmd.Flags().BoolVar(&startOrchWorktrees, "orchestrate-worktrees", false, "use worktrees for orchestrator agents")
}

func pidFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/maquinista.pid"
	}
	dir := filepath.Join(home, ".maquinista")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "maquinista.pid")
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
			return fmt.Errorf("maquinista is already running (PID %d), use 'maquinista stop' first", pid)
		}
		log.Printf("Cleaning up stale PID file (PID %d is dead)", pid)
		removePIDFile()
	}

	// Ensure the Claude Code SessionStart hook is registered.
	if err := hook.EnsureInstalled(); err != nil {
		log.Printf("Warning: failed to ensure hook is installed: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Override default runner if flag is set.
	if startRunner != "" {
		cfg.DefaultRunner = startRunner
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

	// Set the default runner for agent spawning.
	if defaultRunner, rErr := runner.Get(cfg.DefaultRunner); rErr == nil {
		b.SetDefaultRunner(defaultRunner)
		log.Printf("Default runner: %s", cfg.DefaultRunner)
	} else {
		log.Printf("Warning: unknown default runner %q, falling back to claude", cfg.DefaultRunner)
	}

	msPath := filepath.Join(cfg.MaquinistaDir, "monitor_state.json")
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

	claudeSrc := monitor.NewClaudeSource(cfg, b.State(), ms)
	monitor.RegisterSource("claude", claudeSrc)

	opencodeSrc := monitor.NewOpenCodeSource(cfg, b.State(), ms)
	monitor.RegisterSource("opencode", opencodeSrc)

	openclaudeSrc := monitor.NewOpenClaudeSource(cfg, b.State(), ms)
	monitor.RegisterSource("openclaude", openclaudeSrc)

	mon := monitor.New(cfg, b.State(), ms, q)
	mon.AddSource(claudeSrc)
	mon.AddSource(opencodeSrc)
	mon.AddSource(openclaudeSrc)
	mon.PlanHandler = b.HandlePlanFromMonitor

	// Feature flag mailbox.outbound: mirror every captured response into
	// agent_outbox in parallel with the legacy Telegram path.
	if cfg.MailboxOutbound {
		if pool == nil && cfg.DatabaseURL != "" {
			if p, dbErr := db.Connect(cfg.DatabaseURL); dbErr != nil {
				log.Printf("mailbox.outbound: cannot connect DB: %v", dbErr)
			} else {
				pool = p
			}
		}
		if pool != nil {
			mon.OutboxWriter = monitor.NewDBOutboxWriter(pool)
			log.Println("mailbox.outbound: shadow-writing responses to agent_outbox")
		} else {
			log.Println("mailbox.outbound: flag set but no DB pool — ignoring")
		}
	}

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

	// Mailbox inbox consumer: claim agent_inbox rows and pipe content into
	// the pty. Replaces the task-1.6 inboxbridge package. The plan's end
	// state is one sidecar goroutine per live agent (§7); until that per-
	// agent supervisor is wired, this single process consumer drains all
	// agents serially via FOR UPDATE SKIP LOCKED.
	if pool == nil && cfg.DatabaseURL != "" {
		if p, dbErr := db.Connect(cfg.DatabaseURL); dbErr != nil {
			log.Printf("mailbox: cannot connect DB: %v", dbErr)
		} else {
			pool = p
		}
	}
	if pool != nil {
		b.SetPool(pool)
		workerID := fmt.Sprintf("consumer-%d", os.Getpid())
		go runMailboxConsumer(ctx, pool, cfg.TmuxSessionName, workerID)
		log.Printf("mailbox: inbox consumer running (worker=%s)", workerID)

		// Declarative jobreg reconcile: upsert every YAML under
		// config/schedules + config/hooks, soft-disable rows whose
		// YAML is gone. Missing dirs are fine (silent no-op).
		if err := jobreg.Reconcile(ctx, pool, "config/schedules", "config/hooks"); err != nil {
			log.Printf("jobreg reconcile: %v", err)
		}

		// Inject the tier-3 spawn callback into the bot. Per
		// plans/archive/per-topic-agent-pivot.md the daemon no longer spawns a
		// shared default agent at startup; agents are spawned per topic
		// on first message via routing.Resolve → SpawnFunc.
		if cwd, cwdErr := resolveStartCWD(cfg); cwdErr != nil {
			log.Printf("spawn_topic_agent: cwd resolve failed: %v (tier-3 spawn will error)", cwdErr)
		} else {
			b.SetTopicAgentSpawner(newTopicAgentSpawner(cfg, pool, b.State(), cwd))
		}
	} else {
		log.Println("mailbox: DB pool unavailable — inbox routing will error")
	}

	// Start orchestrator if requested.
	if startOrchestrate {
		if pool == nil && cfg.DatabaseURL != "" {
			var dbErr error
			pool, dbErr = db.Connect(cfg.DatabaseURL)
			if dbErr != nil {
				log.Printf("Warning: failed to connect DB for orchestrator: %v", dbErr)
			}
		}
		if pool != nil {
			b.SetPool(pool)
		}
		if pool == nil {
			log.Println("Warning: --orchestrate requires DATABASE_URL for DB pool")
			startOrchestrate = false
		}
	}
	if startOrchestrate {
		orchProject := startOrchProject
		if orchProject == "" {
			orchProject = os.Getenv("MAQUINISTA_PROJECT")
		}
		if orchProject == "" {
			log.Println("Warning: --orchestrate requires --orchestrate-project or MAQUINISTA_PROJECT")
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
