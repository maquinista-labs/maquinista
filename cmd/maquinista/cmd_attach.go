package main

import (
	"fmt"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach [id]",
	Short: "Attach to tmux session or jump to agent/task window",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		session := getSessionName()

		if len(args) == 0 {
			return tmux.AttachOrSwitch(session, "")
		}

		id := args[0]

		// Try as agent ID first.
		agent, err := db.GetAgent(pool, id)
		if err != nil {
			return err
		}
		if agent != nil {
			return tmux.AttachOrSwitch(session, agent.TmuxWindow)
		}

		// Try as task ID (partial match) — find the agent working on it.
		resolvedID, err := db.ResolvePartialID(pool, id)
		if err != nil {
			return fmt.Errorf("no agent or task found matching %q", id)
		}

		taskAgent, err := db.GetAgentByTaskID(pool, resolvedID)
		if err != nil {
			return err
		}
		if taskAgent == nil {
			return fmt.Errorf("no agent is currently working on task %s", resolvedID)
		}

		return tmux.AttachOrSwitch(session, taskAgent.TmuxWindow)
	},
}

func init() {
	rootCmd.AddCommand(attachCmd)
}
