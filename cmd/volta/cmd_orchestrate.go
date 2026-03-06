package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/otaviocarvalho/volta/internal/agent"
	"github.com/otaviocarvalho/volta/internal/listener"
	"github.com/otaviocarvalho/volta/internal/orchestrator"
	"github.com/otaviocarvalho/volta/internal/runner"
	"github.com/otaviocarvalho/volta/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	orchProject      string
	orchMaxAgents    int
	orchPollInterval time.Duration
	orchWorktrees    bool
	orchRunner       string
	orchStatus       bool
)

var orchestrateCmd = &cobra.Command{
	Use:   "orchestrate",
	Short: "Run the autonomous orchestrator loop",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		proj := orchProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}

		// One-shot status query.
		if orchStatus {
			var projPtr *string
			if proj != "" {
				projPtr = &proj
			}
			status, err := orchestrator.Status(pool, projPtr)
			if err != nil {
				return err
			}
			fmt.Println(status)
			return nil
		}

		if proj == "" {
			return fmt.Errorf("--project is required")
		}

		session := getSessionName()
		if err := tmux.EnsureSession(session); err != nil {
			return err
		}

		claudeMD, err := findClaudeMD()
		if err != nil {
			return err
		}

		r, err := runner.Get(orchRunner)
		if err != nil {
			return fmt.Errorf("unknown runner %q: %w", orchRunner, err)
		}

		databaseURL := dbURL
		if databaseURL == "" {
			databaseURL = os.Getenv("DATABASE_URL")
		}

		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		// Start event listener for NOTIFY-driven wake-up.
		el := listener.New(databaseURL)
		go el.Start(ctx)
		notifyCh := orchestrator.NotifyBridge(ctx, el.TaskEvents)

		cfg := orchestrator.Config{
			Pool:         pool,
			Runner:       r,
			TmuxSession:  session,
			ProjectID:    proj,
			MaxAgents:    orchMaxAgents,
			PollInterval: orchPollInterval,
			UseWorktrees: orchWorktrees,
			ClaudeMDPath: claudeMD,
			DatabaseURL:  databaseURL,
			NotifyCh:     notifyCh,
		}

		err = orchestrator.Run(ctx, cfg)

		// Graceful shutdown: kill all spawned agents.
		log.Println("Shutting down: killing all agents...")
		if killErr := agent.KillAll(pool, session); killErr != nil {
			log.Printf("Error killing agents: %v", killErr)
		}

		return err
	},
}

func init() {
	orchestrateCmd.Flags().StringVar(&orchProject, "project", "", "project ID (required)")
	orchestrateCmd.Flags().IntVar(&orchMaxAgents, "max-agents", 3, "maximum concurrent agents")
	orchestrateCmd.Flags().DurationVar(&orchPollInterval, "poll-interval", 10*time.Second, "polling interval")
	orchestrateCmd.Flags().BoolVar(&orchWorktrees, "worktrees", false, "isolate agents in git worktrees")
	orchestrateCmd.Flags().StringVar(&orchRunner, "runner", "claude", "agent runner to use")
	orchestrateCmd.Flags().BoolVar(&orchStatus, "status", false, "show current status and exit")
	rootCmd.AddCommand(orchestrateCmd)
}
