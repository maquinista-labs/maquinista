package main

import (
	"fmt"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var logsLines int

var logsCmd = &cobra.Command{
	Use:   "logs <agent-id>",
	Short: "Capture last N lines from agent's tmux window",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		agentID := args[0]
		agent, err := db.GetAgent(pool, agentID)
		if err != nil {
			return err
		}
		if agent == nil {
			return fmt.Errorf("agent %q not found", agentID)
		}

		output, err := tmux.CapturePaneLines(agent.TmuxSession, agent.TmuxWindow, logsLines)
		if err != nil {
			return fmt.Errorf("capturing pane: %w", err)
		}

		fmt.Println(output)
		return nil
	},
}

func init() {
	logsCmd.Flags().IntVar(&logsLines, "lines", 50, "number of lines to capture")
	rootCmd.AddCommand(logsCmd)
}
