package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/maquinista-labs/maquinista/internal/daemonize"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/spf13/cobra"
)

// Per plans/active/detached-processes.md, the top-level `status`
// command now reports the orchestrator + dashboard daemon state.
// The previous task-table command moved under `maquinista tasks
// status` (see tasksStatusCmd below).

var (
	statusProject string
	statusJSON    bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show orchestrator + dashboard daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		rows := collectDaemonStatus()
		fmt.Fprint(cmd.OutOrStdout(), formatDaemonStatusTable(rows))
		return nil
	},
}

var tasksStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Table view of all tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTasksStatus(cmd.OutOrStdout())
	},
}

func init() {
	tasksStatusCmd.Flags().StringVar(&statusProject, "project", "", "filter by project ID")
	tasksStatusCmd.Flags().BoolVar(&statusJSON, "json", false, "output as JSON")
	tasksCmd.AddCommand(tasksStatusCmd)
	rootCmd.AddCommand(statusCmd)
}

// daemonStatusRow is a single line in the top-level `status` table.
// Separated from the rendering function so tests can feed synthetic
// rows and compare against the .golden file.
type daemonStatusRow struct {
	Name  string
	PID   int
	Alive bool
	Log   string
}

func collectDaemonStatus() []daemonStatusRow {
	rows := make([]daemonStatusRow, 0, 2)
	for _, spec := range []daemonize.Spec{orchestratorSpec(), dashboardSpec()} {
		pid, alive, _ := daemonize.Status(spec)
		rows = append(rows, daemonStatusRow{
			Name:  spec.Name,
			PID:   pid,
			Alive: alive,
			Log:   spec.LogPath,
		})
	}
	return rows
}

// formatDaemonStatusTable renders a deterministic, test-friendly
// table. The "stopped" row shows a "—" for PID so the width stays
// consistent regardless of whether a daemon is running.
func formatDaemonStatusTable(rows []daemonStatusRow) string {
	var buf stringWriter
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMPONENT\tPID\tSTATE\tLOG")
	for _, r := range rows {
		pid := "—"
		state := "stopped"
		if r.Alive {
			pid = strconv.Itoa(r.PID)
			state = "running"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, pid, state, r.Log)
	}
	_ = w.Flush()
	return buf.String()
}

type stringWriter struct{ buf []byte }

func (s *stringWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}
func (s *stringWriter) String() string { return string(s.buf) }

// runTasksStatus is the pre-D.4 `maquinista status` command, now
// `maquinista tasks status`. Unchanged behaviour except the output
// writer is injected so tests don't have to capture stdout.
func runTasksStatus(out io.Writer) error {
	if err := connectDB(); err != nil {
		return err
	}

	proj := statusProject
	if proj == "" {
		proj = os.Getenv("MAQUINISTA_PROJECT")
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
		fmt.Fprintln(out, string(data))
		return nil
	}

	if len(tasks) == 0 {
		fmt.Fprintln(out, "No tasks.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
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
