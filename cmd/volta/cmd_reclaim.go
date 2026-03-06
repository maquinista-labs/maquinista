package main

import (
	"fmt"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var reclaimMinutes int

var reclaimCmd = &cobra.Command{
	Use:   "reclaim",
	Short: "Reset stale claimed tasks back to ready",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		count, err := db.ReclaimStale(pool, reclaimMinutes)
		if err != nil {
			return err
		}

		if count == 0 {
			fmt.Println("No stale tasks to reclaim.")
		} else {
			fmt.Printf("Reclaimed %d stale task(s).\n", count)
		}
		return nil
	},
}

func init() {
	reclaimCmd.Flags().IntVar(&reclaimMinutes, "minutes", 30, "stale threshold in minutes")
	rootCmd.AddCommand(reclaimCmd)
}
