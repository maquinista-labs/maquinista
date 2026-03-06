package main

import (
	"fmt"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var unclaimCmd = &cobra.Command{
	Use:   "unclaim <task-id>",
	Short: "Release a claimed task back to ready",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		resolvedID, err := db.ResolvePartialID(pool, args[0])
		if err != nil {
			return err
		}

		if err := db.UnclaimTask(pool, resolvedID); err != nil {
			return err
		}
		fmt.Printf("Unclaimed: %s\n", resolvedID)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(unclaimCmd)
}
