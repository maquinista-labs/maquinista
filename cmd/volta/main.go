package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/otaviocarvalho/volta/hook"
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
	version     = "dev"
	dbURL       string
	sessionName string
	pool        *pgxpool.Pool
	cfgPath     string
	installHook bool

	// serve --orchestrate flags
	serveOrchestrate    bool
	serveOrchProject    string
	serveOrchMaxAgents  int
	serveOrchRunner     string
	serveOrchWorktrees  bool
)

var rootCmd = &cobra.Command{
	Use:   "volta",
	Short: "Unified agent orchestration platform",
	Long:  "Volta combines Telegram bot management, pull-based task coordination, and pluggable agent runners into a single CLI.",
}

func init() {
	rootCmd.PersistentFlags().StringVar(&dbURL, "db", "", "database URL (overrides DATABASE_URL)")
	rootCmd.PersistentFlags().StringVar(&sessionName, "session", "", "tmux session name (overrides VOLTA_SESSION)")
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "", "config file path")

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(hookCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("volta", version)
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the Telegram bot",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if cfgPath != "" {
			_ = godotenv.Load(cfgPath)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
}

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Run the Claude Code SessionStart hook",
	RunE: func(cmd *cobra.Command, args []string) error {
		if installHook {
			return hook.Install()
		}
		return hook.Run()
	},
}

func init() {
	serveCmd.Flags().StringVar(&cfgPath, "env", "", "path to .env config file")
	serveCmd.Flags().BoolVar(&serveOrchestrate, "orchestrate", false, "run orchestrator alongside bot")
	serveCmd.Flags().StringVar(&serveOrchProject, "orchestrate-project", "", "project for orchestrator")
	serveCmd.Flags().IntVar(&serveOrchMaxAgents, "orchestrate-max-agents", 3, "max agents for orchestrator")
	serveCmd.Flags().StringVar(&serveOrchRunner, "orchestrate-runner", "claude", "runner for orchestrator")
	serveCmd.Flags().BoolVar(&serveOrchWorktrees, "orchestrate-worktrees", false, "use worktrees for orchestrator agents")
	hookCmd.Flags().BoolVar(&installHook, "install", false, "install hook into Claude Code settings")
}

// connectDB initializes the database pool. Call from subcommands that need DB access.
func connectDB() error {
	url := dbURL
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		return fmt.Errorf("DATABASE_URL not set (use --db flag or .env)")
	}

	var err error
	pool, err = db.Connect(url)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	return nil
}

// getSessionName returns the tmux session name from flag, env, or default.
func getSessionName() string {
	if sessionName != "" {
		return sessionName
	}
	if s := os.Getenv("VOLTA_SESSION"); s != "" {
		return s
	}
	return "volta"
}

func runServe() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	b, err := bot.New(cfg)
	if err != nil {
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

	go mon.Run(ctx)
	go sp.Run(ctx)

	// Start orchestrator if requested.
	if serveOrchestrate {
		if pool == nil && cfg.DatabaseURL != "" {
			var dbErr error
			pool, dbErr = db.Connect(cfg.DatabaseURL)
			if dbErr != nil {
				log.Printf("Warning: failed to connect DB for orchestrator: %v", dbErr)
			}
		}
		if pool == nil {
			log.Println("Warning: --orchestrate requires DATABASE_URL for DB pool")
			serveOrchestrate = false
		}
	}
	if serveOrchestrate {
		orchProject := serveOrchProject
		if orchProject == "" {
			orchProject = os.Getenv("VOLTA_PROJECT")
		}
		if orchProject == "" {
			log.Println("Warning: --orchestrate requires --orchestrate-project or VOLTA_PROJECT")
		} else {
			r, rErr := runner.Get(serveOrchRunner)
			if rErr != nil {
				log.Printf("Warning: unknown orchestrator runner %q: %v", serveOrchRunner, rErr)
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
						MaxAgents:    serveOrchMaxAgents,
						PollInterval: 10 * time.Second,
						UseWorktrees: serveOrchWorktrees,
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
						orchProject, serveOrchMaxAgents, serveOrchRunner)
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

func main() {
	_ = godotenv.Load()

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}

	if pool != nil {
		pool.Close()
	}
}
