package main

import (
	"fmt"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run pending database migrations",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		applied, err := db.RunMigrations(pool)
		if err != nil {
			return fmt.Errorf("running migrations: %w", err)
		}

		if len(applied) == 0 {
			fmt.Println("Nothing to apply — all migrations are current.")
			return nil
		}

		for _, name := range applied {
			fmt.Printf("✓ Applied: %s\n", name)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}
