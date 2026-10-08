package main

import (
	"context"
	"fmt"
	"os"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/pipeline"
	"github.com/spf13/cobra"
)

var requeueBy string

var requeueCmd = &cobra.Command{
	Use:   "requeue <task-id>",
	Short: "Requeue a pending_approval task back to ready",
	Long: `Sanctioned manual re-entry after a needs-human park (MAQ-40).

Moves the task from pending_approval back to ready — and only from there
(approve/reject stay the terminal exits). The worktree and branch are
preserved, so the next implementor round updates the same PR instead of
starting a duplicate. A journal entry records who requeued; a courtesy
note lands in the Pipeline topic.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		resolvedID, err := db.ResolvePartialID(pool, args[0])
		if err != nil {
			return err
		}

		actor := requeueBy
		if actor == "" {
			actor = os.Getenv("APPROVER_ID")
		}
		if actor == "" {
			actor = "cli"
		}

		if err := db.RequeueTask(pool, resolvedID, actor); err != nil {
			return err
		}

		// Courtesy note for the Pipeline topic — the state change above is
		// the system of record, a failed notify must not fail the requeue.
		pipeline.NotifyTaskf(context.Background(), pool, resolvedID,
			"🔄 %s: requeued to ready by %s — needs-human park released, next round updates the same PR.",
			resolvedID, actor)

		fmt.Printf("Requeued: %s (by %s)\n", resolvedID, actor)
		return nil
	},
}

func init() {
	requeueCmd.Flags().StringVar(&requeueBy, "by", "", "operator identity (journal traceability)")
	rootCmd.AddCommand(requeueCmd)
}
