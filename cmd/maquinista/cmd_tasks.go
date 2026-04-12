package main

import (
	"context"
	"fmt"

	"github.com/maquinista-labs/maquinista/internal/tools/tasks"
	"github.com/spf13/cobra"
)

var tasksCmd = &cobra.Command{Use: "tasks", Short: "Typed task DAG ops for skills"}

var (
	tasksCreateID, tasksCreateTitle, tasksCreateBody, tasksCreateRole, tasksCreateWorktree string
	tasksDepChild, tasksDepParent                                                         string
	tasksPRID, tasksPRURL                                                                  string
	tasksMarkID                                                                            string
)

var tasksCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "CreateTask: insert a task row",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		t := tasks.Task{
			ID: tasksCreateID, Title: tasksCreateTitle, Body: tasksCreateBody,
			Role: tasksCreateRole,
		}
		if tasksCreateWorktree != "" {
			t.WorktreePath = &tasksCreateWorktree
		}
		return tasks.CreateTask(context.Background(), pool, t)
	},
}

var tasksAddDepCmd = &cobra.Command{
	Use:   "add-dep",
	Short: "AddDep: child depends on parent",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return tasks.AddDep(context.Background(), pool, tasksDepChild, tasksDepParent)
	},
}

var tasksValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "ValidateDAG: fail if any cycle exists",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		if err := tasks.ValidateDAG(context.Background(), pool); err != nil {
			return err
		}
		fmt.Println("DAG is acyclic.")
		return nil
	},
}

var tasksSetPRCmd = &cobra.Command{
	Use:   "set-pr",
	Short: "SetPRUrl: record PR url + flip task to 'review'/'open'",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return tasks.SetPRUrl(context.Background(), pool, tasksPRID, tasksPRURL)
	},
}

var tasksMarkMergedCmd = &cobra.Command{
	Use:   "mark-merged <id>",
	Args:  cobra.ExactArgs(1),
	Short: "MarkMerged: pr_state=merged, status=done (cascades readiness)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return tasks.MarkMerged(context.Background(), pool, args[0])
	},
}

var tasksMarkClosedCmd = &cobra.Command{
	Use:   "mark-closed <id>",
	Args:  cobra.ExactArgs(1),
	Short: "MarkClosed: pr_state=closed, status=failed",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return tasks.MarkClosed(context.Background(), pool, args[0])
	},
}

func init() {
	tasksCreateCmd.Flags().StringVar(&tasksCreateID, "id", "", "")
	tasksCreateCmd.Flags().StringVar(&tasksCreateTitle, "title", "", "")
	tasksCreateCmd.Flags().StringVar(&tasksCreateBody, "body", "", "")
	tasksCreateCmd.Flags().StringVar(&tasksCreateRole, "role", "executor", "")
	tasksCreateCmd.Flags().StringVar(&tasksCreateWorktree, "worktree", "", "")

	tasksAddDepCmd.Flags().StringVar(&tasksDepChild, "child", "", "")
	tasksAddDepCmd.Flags().StringVar(&tasksDepParent, "parent", "", "")

	tasksSetPRCmd.Flags().StringVar(&tasksPRID, "id", "", "")
	tasksSetPRCmd.Flags().StringVar(&tasksPRURL, "url", "", "")

	tasksCmd.AddCommand(tasksCreateCmd, tasksAddDepCmd, tasksValidateCmd,
		tasksSetPRCmd, tasksMarkMergedCmd, tasksMarkClosedCmd)
	rootCmd.AddCommand(tasksCmd)
}
