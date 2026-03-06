package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
)

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage recurring job schedules",
}

var (
	schedAddCron        string
	schedAddTemplate    string
	schedAddProject     string
	schedAddDescription string
)

var scheduleAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Create a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		name := args[0]

		// Validate cron expression.
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		sched, err := parser.Parse(schedAddCron)
		if err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", schedAddCron, err)
		}

		// Read template file.
		data, err := os.ReadFile(schedAddTemplate)
		if err != nil {
			return fmt.Errorf("reading template: %w", err)
		}

		// Validate template is valid JSON array.
		var nodes []json.RawMessage
		if err := json.Unmarshal(data, &nodes); err != nil {
			return fmt.Errorf("template must be a JSON array: %w", err)
		}

		var projPtr *string
		proj := schedAddProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}
		if proj != "" {
			projPtr = &proj
		}

		var descPtr *string
		if schedAddDescription != "" {
			descPtr = &schedAddDescription
		}

		nextRun := sched.Next(time.Now())

		if err := db.CreateSchedule(pool, name, schedAddCron, data, projPtr, descPtr, nextRun); err != nil {
			return err
		}

		fmt.Printf("Created schedule %q (next run: %s)\n", name, nextRun.Format("2006-01-02 15:04:05"))
		return nil
	},
}

var schedListProject string

var scheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		var projPtr *string
		proj := schedListProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}
		if proj != "" {
			projPtr = &proj
		}

		schedules, err := db.ListSchedules(pool, projPtr)
		if err != nil {
			return err
		}

		if len(schedules) == 0 {
			fmt.Println("No schedules.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "NAME\tCRON\tNEXT RUN\tLAST RUN\tENABLED\n")
		for _, s := range schedules {
			nextRun := "—"
			if s.NextRun != nil {
				nextRun = s.NextRun.Format("2006-01-02 15:04:05")
			}
			lastRun := "—"
			if s.LastRun != nil {
				lastRun = s.LastRun.Format("2006-01-02 15:04:05")
			}
			enabled := "yes"
			if !s.Enabled {
				enabled = "no"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Cron, nextRun, lastRun, enabled)
		}
		w.Flush()
		return nil
	},
}

var scheduleRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Immediately instantiate a schedule's template",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		sched, err := db.GetSchedule(pool, args[0])
		if err != nil {
			return err
		}

		ids, err := instantiateTemplate(sched.Template, sched.ProjectID)
		if err != nil {
			return err
		}

		for _, id := range ids {
			fmt.Println(id)
		}
		return nil
	},
}

var scheduleDisableCmd = &cobra.Command{
	Use:   "disable <name>",
	Short: "Disable a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return db.SetScheduleEnabled(pool, args[0], false, nil)
	},
}

var scheduleEnableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Enable a schedule (recomputes next_run)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		sched, err := db.GetSchedule(pool, args[0])
		if err != nil {
			return err
		}

		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		cronSched, err := parser.Parse(sched.Cron)
		if err != nil {
			return err
		}
		nextRun := cronSched.Next(time.Now())

		return db.SetScheduleEnabled(pool, args[0], true, &nextRun)
	},
}

// TemplateNode is a single node in a schedule template.
type TemplateNode struct {
	Ref              string   `json:"ref"`
	Title            string   `json:"title"`
	Body             string   `json:"body"`
	Priority         int      `json:"priority"`
	TestCmd          string   `json:"test_cmd"`
	RequiresApproval bool     `json:"requires_approval"`
	After            []string `json:"after"`
}

// instantiateTemplate creates draft tasks from a template.
func instantiateTemplate(templateJSON json.RawMessage, projectID *string) ([]string, error) {
	var nodes []TemplateNode
	if err := json.Unmarshal(templateJSON, &nodes); err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	// Map ref → created task ID.
	refMap := make(map[string]string)
	var createdIDs []string

	for _, node := range nodes {
		id := generateID(node.Title)

		var metadata json.RawMessage
		if node.TestCmd != "" {
			m := map[string]string{"test_cmd": node.TestCmd}
			metadata, _ = json.Marshal(m)
		}

		priority := node.Priority
		if priority == 0 {
			priority = 5
		}

		if err := db.CreateTask(pool, id, node.Title, node.Body, priority, projectID, metadata, node.RequiresApproval); err != nil {
			return createdIDs, fmt.Errorf("creating task %q: %w", node.Title, err)
		}

		// Add dependencies.
		for _, depRef := range node.After {
			depID, ok := refMap[depRef]
			if !ok {
				return createdIDs, fmt.Errorf("unknown ref %q in after for %q", depRef, node.Ref)
			}
			if err := db.AddDependency(pool, id, depID); err != nil {
				return createdIDs, err
			}
		}

		// All tasks created as draft.
		if err := db.SetTaskStatus(pool, id, "draft"); err != nil {
			return createdIDs, err
		}

		refMap[node.Ref] = id
		createdIDs = append(createdIDs, id)
	}

	return createdIDs, nil
}

func init() {
	scheduleAddCmd.Flags().StringVar(&schedAddCron, "cron", "", "cron expression (required)")
	scheduleAddCmd.Flags().StringVar(&schedAddTemplate, "template", "", "path to template JSON file (required)")
	scheduleAddCmd.Flags().StringVar(&schedAddProject, "project", "", "project ID")
	scheduleAddCmd.Flags().StringVar(&schedAddDescription, "description", "", "schedule description")
	scheduleAddCmd.MarkFlagRequired("cron")
	scheduleAddCmd.MarkFlagRequired("template")

	scheduleListCmd.Flags().StringVar(&schedListProject, "project", "", "filter by project")

	scheduleCmd.AddCommand(scheduleAddCmd, scheduleListCmd, scheduleRunCmd, scheduleDisableCmd, scheduleEnableCmd)
	rootCmd.AddCommand(scheduleCmd)
}
