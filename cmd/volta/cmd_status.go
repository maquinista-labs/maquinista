package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var (
	statusProject string
	statusJSON    bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Table view of all tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		proj := statusProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}
		var projPtr *string
		if proj != "" {
			projPtr = &proj
		}

		tasks, err := db.ListTasks(pool, projPtr)
		if err != nil {
			return err
		}

		if statusJSON {
			if tasks == nil {
				tasks = []*db.Task{}
			}
			data, err := json.MarshalIndent(tasks, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling JSON: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}

		if len(tasks) == 0 {
			fmt.Println("No tasks.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "  \tID\tTITLE\tSTATUS\tCLAIMED BY\tATTEMPT\n")
		for _, t := range tasks {
			sym := statusSymbol(t.Status)
			claimedBy := "—"
			if t.ClaimedBy != nil {
				claimedBy = *t.ClaimedBy
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d/%d\n",
				sym, truncateID(t.ID), t.Title, t.Status, claimedBy, t.Attempt, t.MaxAttempts)
		}
		w.Flush()
		return nil
	},
}

func init() {
	statusCmd.Flags().StringVar(&statusProject, "project", "", "filter by project ID")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "output as JSON")
	rootCmd.AddCommand(statusCmd)
}

func statusSymbol(status string) string {
	switch status {
	case "pending":
		return "○"
	case "draft":
		return "◌"
	case "ready":
		return "◎"
	case "claimed":
		return "●"
	case "done":
		return "✓"
	case "failed":
		return "✗"
	case "pending_approval":
		return "⊘"
	case "rejected":
		return "⊗"
	default:
		return "?"
	}
}

func truncateID(id string) string {
	if len(id) > 20 {
		return id[:20]
	}
	return id
}
