package main

import (
	"fmt"
	"strings"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Full-text search across task context",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		query := strings.Join(args, " ")
		results, err := db.SearchContext(pool, query)
		if err != nil {
			return err
		}

		if len(results) == 0 {
			fmt.Println("No results.")
			return nil
		}

		for _, c := range results {
			ts := c.CreatedAt.Local().Format("2006-01-02 15:04:05")
			agent := "—"
			if c.AgentID != nil {
				agent = *c.AgentID
			}
			fmt.Printf("[%s] %s  task:%s  agent:%s\n", ts, strings.ToUpper(c.Kind), c.TaskID, agent)

			// Show a snippet of the content (first 200 chars).
			snippet := c.Content
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			for _, line := range strings.Split(snippet, "\n") {
				fmt.Printf("  %s\n", line)
			}
			fmt.Println()
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(searchCmd)
}
