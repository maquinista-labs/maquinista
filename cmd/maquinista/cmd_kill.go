package main

import (
	"fmt"

	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/spf13/cobra"
)

var killAll bool

var killCmd = &cobra.Command{
	Use:   "kill [agent-id]",
	Short: "Kill agent(s), release their claimed tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		session := getSessionName()

		if killAll {
			if err := agent.KillAll(pool, session); err != nil {
				return err
			}
			fmt.Println("✓ All agents killed.")
			return nil
		}

		if len(args) == 0 {
			return fmt.Errorf("specify an agent ID or use --all")
		}

		agentID := args[0]
		if err := agent.Kill(pool, session, agentID); err != nil {
			return err
		}

		fmt.Printf("✓ Killed agent %s\n", agentID)
		return nil
	},
}

func init() {
	killCmd.Flags().BoolVar(&killAll, "all", false, "kill all agents")
	rootCmd.AddCommand(killCmd)
}
