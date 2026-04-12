package main

import (
	"fmt"
	"os"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/prompt"
	"github.com/spf13/cobra"
)

var promptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "Generate self-contained prompts for Claude agents",
}

// --- single ---

var promptSingleCmd = &cobra.Command{
	Use:   "single <task-id>",
	Short: "Output a single-task prompt",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		task, ctxs, err := db.GetTaskWithContext(pool, args[0])
		if err != nil {
			return err
		}

		fmt.Println(prompt.BuildSinglePrompt(task, ctxs))
		return nil
	},
}

// --- auto ---

var autoProject string

var promptAutoCmd = &cobra.Command{
	Use:   "auto",
	Short: "Output a loop prompt for auto mode",
	RunE: func(cmd *cobra.Command, args []string) error {
		proj := autoProject
		if proj == "" {
			proj = os.Getenv("MAQUINISTA_PROJECT")
		}
		if proj == "" {
			return fmt.Errorf("--project is required for auto mode")
		}

		fmt.Println(prompt.BuildAutoPrompt(proj))
		return nil
	},
}

// --- batch ---

var promptBatchCmd = &cobra.Command{
	Use:   "batch <id1> [id2] ...",
	Short: "Output a multi-task batch prompt",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		var entries []prompt.TaskWithContext
		for _, id := range args {
			task, ctxs, err := db.GetTaskWithContext(pool, id)
			if err != nil {
				return fmt.Errorf("loading task %q: %w", id, err)
			}
			entries = append(entries, prompt.TaskWithContext{Task: task, Ctxs: ctxs})
		}

		fmt.Println(prompt.BuildBatchPrompt(entries))
		return nil
	},
}

func init() {
	promptAutoCmd.Flags().StringVar(&autoProject, "project", "", "project to claim from (required)")
	promptCmd.AddCommand(promptSingleCmd)
	promptCmd.AddCommand(promptAutoCmd)
	promptCmd.AddCommand(promptBatchCmd)
	rootCmd.AddCommand(promptCmd)
}
