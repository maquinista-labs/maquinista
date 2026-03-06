package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/otaviocarvalho/volta/internal/tui"
	"github.com/spf13/cobra"
)

var agentsWatch bool

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Show running agents + what they own",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		if agentsWatch {
			return watchAgents()
		}

		return printAgents()
	},
}

func init() {
	agentsCmd.Flags().BoolVar(&agentsWatch, "watch", false, "refresh every 2s")
	rootCmd.AddCommand(agentsCmd)
}

func printAgents() error {
	agents, err := db.ListAgents(pool)
	if err != nil {
		return err
	}

	if len(agents) == 0 {
		fmt.Println("No agents.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  \tAGENT\tSTATUS\tTASK\tBRANCH\tLAST SEEN\n")
	for _, a := range agents {
		sym := "○"
		if a.Status == "working" {
			sym = "●"
		}
		taskID := "—"
		if a.TaskID != nil {
			taskID = *a.TaskID
		}
		branch := "—"
		if a.Branch != nil {
			branch = *a.Branch
		}
		lastSeen := "—"
		if a.LastSeen != nil {
			lastSeen = relativeTime(*a.LastSeen)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", sym, a.ID, a.Status, taskID, branch, lastSeen)
	}
	w.Flush()
	return nil
}

func watchAgents() error {
	return tui.Run(pool)
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
