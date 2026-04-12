package main

import (
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/maquinista-labs/maquinista/hook"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	dbURL       string
	sessionName string
	pool        *pgxpool.Pool
	cfgPath     string
	installHook bool
)

var rootCmd = &cobra.Command{
	Use:   "maquinista",
	Short: "Unified agent orchestration platform",
	Long:  "Maquinista combines Telegram bot management, pull-based task coordination, and pluggable agent runners into a single CLI.",
}

func init() {
	rootCmd.PersistentFlags().StringVar(&dbURL, "db", "", "database URL (overrides DATABASE_URL)")
	rootCmd.PersistentFlags().StringVar(&sessionName, "session", "", "tmux session name (overrides MAQUINISTA_SESSION)")
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "", "config file path")

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(hookCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("maquinista", version)
	},
}

var serveCmd = &cobra.Command{
	Use:    "serve",
	Short:  "Start the Telegram bot (alias for 'start')",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStart()
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
	if s := os.Getenv("MAQUINISTA_SESSION"); s != "" {
		return s
	}
	return "maquinista"
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
