package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/maquinista-labs/maquinista/internal/jobreg"
	"github.com/spf13/cobra"
)

var (
	schedName, schedCron, schedAgent, schedPromptTxt, schedTZ, schedWarm string
	hookName, hookPath, hookSecret, hookAgent, hookTemplate, hookScheme  string
	hookRate                                                             int
)

// jobSchedule* is the jobreg (Appendix C) surface. A pre-existing top-level
// `schedule` command belongs to the legacy Minuano scheduler — we add a
// sibling `job-schedule` subtree so both coexist.
var jobScheduleCmd = &cobra.Command{Use: "job-schedule", Short: "Manage Appendix C.2 scheduled_jobs"}

var jobScheduleAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Upsert a scheduled job",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		s := jobreg.Schedule{
			Name: schedName, Cron: schedCron, AgentID: schedAgent,
			Timezone: schedTZ, WarmSpawnBefore: schedWarm,
			Prompt: map[string]any{"type": "command", "text": schedPromptTxt},
		}
		id, err := jobreg.AddSchedule(context.Background(), pool, s)
		if err != nil {
			return err
		}
		fmt.Printf("Upserted schedule %q (id=%s)\n", schedName, id)
		return nil
	},
}

var jobScheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		list, err := jobreg.ListSchedules(context.Background(), pool)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(list)
	},
}

var jobScheduleRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Delete a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return jobreg.RmSchedule(context.Background(), pool, args[0])
	},
}

var hookCmdGroup = &cobra.Command{Use: "hook", Short: "Manage webhook_handlers"}

var hookAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Upsert a webhook handler",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		h := jobreg.Hook{
			Name: hookName, Path: hookPath, Secret: hookSecret,
			SignatureScheme: hookScheme, AgentID: hookAgent,
			PromptTemplate:  hookTemplate, RateLimitPerMin: hookRate,
		}
		id, err := jobreg.AddHook(context.Background(), pool, h)
		if err != nil {
			return err
		}
		fmt.Printf("Upserted hook %q (id=%s)\n", hookName, id)
		return nil
	},
}

var hookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all webhook handlers",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		list, err := jobreg.ListHooks(context.Background(), pool)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(list)
	},
}

var hookRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Delete a webhook handler",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return jobreg.RmHook(context.Background(), pool, args[0])
	},
}

var hookEnableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Enable a webhook handler",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return jobreg.SetHookEnabled(context.Background(), pool, args[0], true)
	},
}

var hookDisableCmd = &cobra.Command{
	Use:   "disable <name>",
	Short: "Disable a webhook handler",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return jobreg.SetHookEnabled(context.Background(), pool, args[0], false)
	},
}

func init() {
	jobScheduleAddCmd.Flags().StringVar(&schedName, "name", "", "")
	jobScheduleAddCmd.Flags().StringVar(&schedCron, "cron", "", "")
	jobScheduleAddCmd.Flags().StringVar(&schedAgent, "agent", "", "")
	jobScheduleAddCmd.Flags().StringVar(&schedPromptTxt, "prompt", "", "")
	jobScheduleAddCmd.Flags().StringVar(&schedTZ, "tz", "UTC", "")
	jobScheduleAddCmd.Flags().StringVar(&schedWarm, "warm", "", "pg interval (e.g. \"10 minutes\")")

	hookAddCmd.Flags().StringVar(&hookName, "name", "", "")
	hookAddCmd.Flags().StringVar(&hookPath, "path", "", "")
	hookAddCmd.Flags().StringVar(&hookSecret, "secret", "", "")
	hookAddCmd.Flags().StringVar(&hookAgent, "agent", "", "")
	hookAddCmd.Flags().StringVar(&hookTemplate, "template", "", "")
	hookAddCmd.Flags().StringVar(&hookScheme, "scheme", "github-hmac-sha256", "")
	hookAddCmd.Flags().IntVar(&hookRate, "rate", 60, "")

	jobScheduleCmd.AddCommand(jobScheduleAddCmd, jobScheduleListCmd, jobScheduleRmCmd)
	hookCmdGroup.AddCommand(hookAddCmd, hookListCmd, hookRmCmd, hookEnableCmd, hookDisableCmd)
	rootCmd.AddCommand(jobScheduleCmd, hookCmdGroup)
}
