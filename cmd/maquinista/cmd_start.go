package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// Per plans/active/detached-processes.md, the orchestrator daemon
// lives under `maquinista orchestrator start` (see
// cmd_orchestrator.go). Top-level `start` is temporarily kept as a
// deprecation shim that delegates to the same supervisor body. D.4
// replaces it with a two-daemon (orchestrator + dashboard) bootstrap.

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
	Short: "Start the orchestrator daemon (deprecated alias for `orchestrator start`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(os.Stderr, "warning: `maquinista start` is a deprecated alias; use `maquinista orchestrator start` for the bot daemon (D.4 will repurpose `maquinista start` for the full stack).")
		return runOrchestratorStart(cmd.Context())
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
	startCmd.Flags().BoolVarP(&orchestratorStartForeground, "foreground", "F", false, "run in the current terminal (default: detach and return immediately)")
}

// runOrchestratorSupervised is the bot + monitor + mailbox +
// optional-orchestrator engine body. It used to live in a function
// called runStart(); post-D.3 it's invoked from
// daemonize.Run's foreground branch via runOrchestratorStart. The
// ctx is pre-wired with SIGINT/SIGTERM handling by daemonize.
func runOrchestratorSupervised(ctx context.Context) error {
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

	b, err := bot.New(cfg)
	if err != nil {
		return fmt.Errorf("creating bot: %w", err)
	}

	// Set the default runner for agent spawning.
	if defaultRunner, rErr := runner.Get(cfg.DefaultRunner); rErr == nil {
		b.SetDefaultRunner(defaultRunner)
		log.Printf("Default runner: %s", cfg.DefaultRunner)
	} else {
		log.Printf("Warning: unknown default runner %q, falling back to claude", cfg.DefaultRunner)
	}

	// One-time cleanup: the legacy session_map.json and the soul prompts/
	// directory are both retired (Phase A of json-state-migration.md, and
	// §0 compliance for agent souls). The file/dir no longer exists on
	// fresh installs; this removes stragglers for existing installs. Safe
	// to ignore "does not exist" errors.
	_ = os.Remove(filepath.Join(cfg.MaquinistaDir, "session_map.json"))
	_ = os.Remove(filepath.Join(cfg.MaquinistaDir, "session_map.json.lock"))
	_ = os.RemoveAll(filepath.Join(cfg.MaquinistaDir, "prompts"))

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

	// Ensure a DB pool is available before constructing monitor sources — the
	// Phase A DB-backed session discovery requires it. Sources tolerate a
	// nil pool (no sessions discovered until the pool lands).
	if pool == nil && cfg.DatabaseURL != "" {
		if p, dbErr := db.Connect(cfg.DatabaseURL); dbErr != nil {
			log.Printf("monitor: cannot connect DB: %v", dbErr)
		} else {
			pool = p
		}
	}

	// Phase B of json-state-migration: let state.State route its read/write
	// paths through Postgres when a pool is available. Without this, the
	// DB-backed implementations fall through to the in-memory JSON maps.
	if pool != nil {
		b.State().SetPool(pool)
		// state.json is retired once the DB is the system of record. Clean
		// up stragglers from pre-Phase-B installs so operators stop
		// inspecting a stale file that no longer reflects reality.
		_ = os.Remove(filepath.Join(cfg.MaquinistaDir, "state.json"))

		// Respawn live agents that survived a previous `maquinista stop`.
		// Uses --resume <session_id> when the hook has recorded one so
		// Claude's context carries across restarts.
		if cwd, cwdErr := resolveStartCWD(cfg); cwdErr == nil {
			respawnCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if n, err := reconcileAgentPanes(respawnCtx, cfg, pool, b.State(), cwd); err != nil {
				log.Printf("reconcile: %v", err)
			} else if n > 0 {
				log.Printf("reconcile: respawned %d agent pane(s) after restart", n)
			}
			cancel()
		}
	}

	claudeSrc := monitor.NewClaudeSource(cfg, pool, b.State(), ms)
	monitor.RegisterSource("claude", claudeSrc)

	opencodeSrc := monitor.NewOpenCodeSource(cfg, pool, b.State(), ms)
	monitor.RegisterSource("opencode", opencodeSrc)

	openclaudeSrc := monitor.NewOpenClaudeSource(cfg, pool, b.State(), ms)
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
