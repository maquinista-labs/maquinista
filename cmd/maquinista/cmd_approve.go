package main

import (
	"context"
	"fmt"
	"os"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/gh"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
	"github.com/spf13/cobra"
)

var approveBy string

var approveCmd = &cobra.Command{
	Use:   "approve <task-id>",
	Short: "Approve a pending_approval task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		resolvedID, err := db.ResolvePartialID(pool, args[0])
		if err != nil {
			return err
		}

		actor := approveBy
		if actor == "" {
			actor = os.Getenv("APPROVER_ID")
		}
		if actor == "" {
			actor = "cli"
		}

		// gh-mode merge arm: approving a ready_to_merge task runs the
		// PR merge flow now (bypasses the auto-merge gate for this one
		// merge). The pending_approval path below is untouched.
		if t, gerr := db.GetTask(pool, resolvedID); gerr == nil && t.Status == "ready_to_merge" {
			if mCfg := pipeline.MergeConfigFromEnv(); mCfg.Mode == pipeline.MergeModeGH {
				tCfg := pipeline.FromEnv()
				prov, perr := pipeline.NewProvider(tCfg.Provider, tCfg.APIKey)
				if perr != nil {
					prov = nil
				}
				mCfg.Gh = gh.New()
				if err := pipeline.RunMergeOnApprove(context.Background(), pool, mCfg, prov, tCfg.TeamID, resolvedID); err != nil {
					return err
				}
				fmt.Printf("Merged: %s (PR squash-merged, by %s)\n", resolvedID, actor)
				return nil
			}
		}

		if err := db.ApproveTask(pool, resolvedID, actor); err != nil {
			return err
		}
		fmt.Printf("Approved: %s (by %s)\n", resolvedID, actor)
		return nil
	},
}

var rejectReason string

var rejectCmd = &cobra.Command{
	Use:   "reject <task-id>",
	Short: "Reject a pending_approval task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		resolvedID, err := db.ResolvePartialID(pool, args[0])
		if err != nil {
			return err
		}

		if err := db.RejectTask(pool, resolvedID, rejectReason); err != nil {
			return err
		}

		msg := fmt.Sprintf("Rejected: %s", resolvedID)
		if rejectReason != "" {
			msg += fmt.Sprintf(" (%s)", rejectReason)
		}
		fmt.Println(msg)
		return nil
	},
}

var (
	draftReleaseAll     bool
	draftReleaseProject string
)

var draftReleaseCmd = &cobra.Command{
	Use:   "draft-release [task-id]",
	Short: "Release draft tasks for execution",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		if draftReleaseAll {
			proj := draftReleaseProject
			if proj == "" {
				proj = os.Getenv("MAQUINISTA_PROJECT")
			}
			if proj == "" {
				return fmt.Errorf("--project is required with --all")
			}

			n, err := db.DraftReleaseAll(pool, proj)
			if err != nil {
				return err
			}
			fmt.Printf("Released %d draft tasks in project %s\n", n, proj)
			return nil
		}

		if len(args) == 0 {
			return fmt.Errorf("specify a task ID or use --all --project <id>")
		}

		resolvedID, err := db.ResolvePartialID(pool, args[0])
		if err != nil {
			return err
		}

		if err := db.DraftRelease(pool, resolvedID); err != nil {
			return err
		}
		fmt.Printf("Released: %s\n", resolvedID)
		return nil
	},
}

func init() {
	approveCmd.Flags().StringVar(&approveBy, "by", "", "approver identity")
	rootCmd.AddCommand(approveCmd)

	rejectCmd.Flags().StringVar(&rejectReason, "reason", "", "rejection reason")
	rootCmd.AddCommand(rejectCmd)

	draftReleaseCmd.Flags().BoolVar(&draftReleaseAll, "all", false, "release all draft tasks in the project")
	draftReleaseCmd.Flags().StringVar(&draftReleaseProject, "project", "", "project ID (required with --all)")
	rootCmd.AddCommand(draftReleaseCmd)
}
