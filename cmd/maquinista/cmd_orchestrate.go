package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/listener"
	"github.com/maquinista-labs/maquinista/internal/orchestrator"
	"github.com/maquinista-labs/maquinista/internal/runner"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var (
	orchProject      string
	orchMaxAgents    int
	orchPollInterval time.Duration
	orchWorktrees    bool
	orchRunner       string
	orchStatus       bool
	orchNotifyTopic  int
	orchNotifyChat   int64
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
			proj = os.Getenv("MAQUINISTA_PROJECT")
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

		// Set up Telegram notifications if --notify-topic is provided.
		if orchNotifyTopic > 0 && orchNotifyChat != 0 {
			token := os.Getenv("TELEGRAM_BOT_TOKEN")
			if token != "" {
				botAPI, botErr := tgbotapi.NewBotAPI(token)
				if botErr != nil {
					log.Printf("Warning: failed to create bot for notifications: %v", botErr)
				} else {
					cfg.NotifyFunc = func(message string) {
						params := tgbotapi.Params{}
						params.AddNonZero64("chat_id", orchNotifyChat)
						params.AddNonEmpty("text", message)
						params.AddNonZero("message_thread_id", orchNotifyTopic)
						if _, err := botAPI.MakeRequest("sendMessage", params); err != nil {
							log.Printf("Notify error: %v", err)
						}
					}
				}
			} else {
				log.Println("Warning: --notify-topic set but TELEGRAM_BOT_TOKEN not found")
			}
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
	orchestrateCmd.Flags().StringVar(&orchRunner, "runner", "claude", "agent runner to use (claude, openclaude, opencode)")
	orchestrateCmd.Flags().BoolVar(&orchStatus, "status", false, "show current status and exit")
	orchestrateCmd.Flags().IntVar(&orchNotifyTopic, "notify-topic", 0, "Telegram thread ID for notifications")
	orchestrateCmd.Flags().Int64Var(&orchNotifyChat, "notify-chat", 0, "Telegram chat ID for notifications")
	rootCmd.AddCommand(orchestrateCmd)
}
