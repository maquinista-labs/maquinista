package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var plannerCmd = &cobra.Command{
	Use:   "planner",
	Short: "Manage planner sessions",
}

var (
	plannerStartTopic   string
	plannerStartProject string
)

var plannerStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a planner session in a tmux window",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		topicID, err := strconv.ParseInt(plannerStartTopic, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid topic ID: %w", err)
		}

		proj := plannerStartProject
		if proj == "" {
			proj = os.Getenv("MAQUINISTA_PROJECT")
		}
		if proj == "" {
			return fmt.Errorf("--project is required")
		}

		// Check for existing session.
		existing, err := db.GetPlannerSession(pool, topicID)
		if err != nil {
			return err
		}
		if existing != nil && existing.Status == "running" {
			return fmt.Errorf("planner already running for topic %d", topicID)
		}

		windowName := fmt.Sprintf("planner-%d", topicID)
		session := os.Getenv("MAQUINISTA_SESSION")
		if session == "" {
			session = "maquinista"
		}

		// Ensure tmux session exists.
		if err := tmux.EnsureSession(session); err != nil {
			return err
		}

		// Create tmux window.
		env := map[string]string{
			"DATABASE_URL":    dbURL,
			"MAQUINISTA_PROJECT": proj,
		}
		if dbURL == "" {
			env["DATABASE_URL"] = os.Getenv("DATABASE_URL")
		}

		if err := tmux.NewWindowWithDir(session, windowName, ".", env); err != nil {
			return fmt.Errorf("creating planner window: %w", err)
		}

		// Upsert planner session.
		if err := db.UpsertPlannerSession(pool, topicID, proj, windowName, "running"); err != nil {
			tmux.KillWindow(session, windowName)
			return err
		}

		// Send Claude Code with planner system prompt.
		plannerPromptPath := findPlannerPrompt()
		tmux.SendKeys(session, windowName,
			fmt.Sprintf("claude --dangerously-skip-permissions -p \"$(cat %s)\"", plannerPromptPath))

		fmt.Printf("Planner started: topic=%d window=%s project=%s\n", topicID, windowName, proj)
		return nil
	},
}

var plannerStopTopic string

var plannerStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a planner session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		topicID, err := strconv.ParseInt(plannerStopTopic, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid topic ID: %w", err)
		}

		session, err := db.GetPlannerSession(pool, topicID)
		if err != nil {
			return err
		}
		if session == nil {
			return fmt.Errorf("no planner session for topic %d", topicID)
		}

		// Kill tmux window.
		tmuxSession := os.Getenv("MAQUINISTA_SESSION")
		if tmuxSession == "" {
			tmuxSession = "maquinista"
		}
		if session.TmuxWindow != nil {
			tmux.KillWindow(tmuxSession, *session.TmuxWindow)
		}

		if err := db.StopPlannerSession(pool, topicID); err != nil {
			return err
		}

		fmt.Printf("Planner stopped: topic=%d\n", topicID)
		return nil
	},
}

var plannerReopenTopic string

var plannerReopenCmd = &cobra.Command{
	Use:   "reopen",
	Short: "Reopen a stopped or crashed planner session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		topicID, err := strconv.ParseInt(plannerReopenTopic, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid topic ID: %w", err)
		}

		windowName := fmt.Sprintf("planner-%d", topicID)
		tmuxSession := os.Getenv("MAQUINISTA_SESSION")
		if tmuxSession == "" {
			tmuxSession = "maquinista"
		}

		session, ropErr := db.ReopenPlannerSession(pool, topicID, windowName)
		if ropErr != nil {
			return ropErr
		}

		// Ensure tmux session exists.
		if err := tmux.EnsureSession(tmuxSession); err != nil {
			return err
		}

		// Create tmux window.
		env := map[string]string{
			"DATABASE_URL": dbURL,
		}
		if dbURL == "" {
			env["DATABASE_URL"] = os.Getenv("DATABASE_URL")
		}
		if session.ProjectID != nil {
			env["MAQUINISTA_PROJECT"] = *session.ProjectID
		}

		if err := tmux.NewWindowWithDir(tmuxSession, windowName, ".", env); err != nil {
			return fmt.Errorf("creating planner window: %w", err)
		}

		plannerPromptPath := findPlannerPrompt()
		tmux.SendKeys(tmuxSession, windowName,
			fmt.Sprintf("claude --dangerously-skip-permissions -p \"$(cat %s)\"", plannerPromptPath))

		fmt.Printf("Planner reopened: topic=%d window=%s\n", topicID, windowName)
		return nil
	},
}

var plannerStatusProject string

var plannerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show planner sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		var projPtr *string
		proj := plannerStatusProject
		if proj == "" {
			proj = os.Getenv("MAQUINISTA_PROJECT")
		}
		if proj != "" {
			projPtr = &proj
		}

		sessions, err := db.ListPlannerSessions(pool, projPtr)
		if err != nil {
			return err
		}

		if len(sessions) == 0 {
			fmt.Println("No planner sessions.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "TOPIC ID\tPROJECT\tWINDOW\tSTATUS\tSTARTED AT\n")
		for _, s := range sessions {
			proj := ""
			if s.ProjectID != nil {
				proj = *s.ProjectID
			}
			win := ""
			if s.TmuxWindow != nil {
				win = *s.TmuxWindow
			}
			started := ""
			if s.StartedAt != nil {
				started = s.StartedAt.Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", s.TopicID, proj, win, s.Status, started)
		}
		w.Flush()
		return nil
	},
}

func findPlannerPrompt() string {
	// Look for planner system prompt relative to binary or cwd.
	candidates := []string{
		"claude/planner-system-prompt.md",
		"/home/otavio/code/maquinista/claude/planner-system-prompt.md",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0] // fallback
}

func init() {
	plannerStartCmd.Flags().StringVar(&plannerStartTopic, "topic", "", "Telegram thread ID")
	plannerStartCmd.Flags().StringVar(&plannerStartProject, "project", "", "project ID")
	plannerStartCmd.MarkFlagRequired("topic")

	plannerStopCmd.Flags().StringVar(&plannerStopTopic, "topic", "", "Telegram thread ID")
	plannerStopCmd.MarkFlagRequired("topic")

	plannerReopenCmd.Flags().StringVar(&plannerReopenTopic, "topic", "", "Telegram thread ID")
	plannerReopenCmd.MarkFlagRequired("topic")

	plannerStatusCmd.Flags().StringVar(&plannerStatusProject, "project", "", "filter by project")

	plannerCmd.AddCommand(plannerStartCmd, plannerStopCmd, plannerReopenCmd, plannerStatusCmd)
	rootCmd.AddCommand(plannerCmd)
}
