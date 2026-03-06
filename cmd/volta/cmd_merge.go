package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/otaviocarvalho/volta/internal/git"
	"github.com/spf13/cobra"
)

var mergeWatch bool

var mergeCmd = &cobra.Command{
	Use:   "merge",
	Short: "Process merge queue entries",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		if mergeWatch {
			return mergeWatchLoop()
		}
		return mergeOne()
	},
}

var mergeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show merge queue status",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		return printMergeQueue()
	},
}

func init() {
	mergeCmd.Flags().BoolVar(&mergeWatch, "watch", false, "poll every 5s and process continuously")
	mergeCmd.AddCommand(mergeStatusCmd)
	rootCmd.AddCommand(mergeCmd)
}

func mergeOne() error {
	entry, err := db.ClaimMergeEntry(pool)
	if err != nil {
		return fmt.Errorf("claiming merge entry: %w", err)
	}
	if entry == nil {
		fmt.Println("No pending merge entries.")
		return nil
	}

	return processMerge(entry)
}

func mergeWatchLoop() error {
	fmt.Println("Watching merge queue (Ctrl+C to stop)...")
	for {
		entry, err := db.ClaimMergeEntry(pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error claiming entry: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if entry == nil {
			time.Sleep(5 * time.Second)
			continue
		}

		if err := processMerge(entry); err != nil {
			fmt.Fprintf(os.Stderr, "error processing merge: %v\n", err)
		}
	}
}

func processMerge(entry *db.MergeQueueEntry) error {
	fmt.Printf("Merging: %s (task %s, branch %s → %s)\n", fmt.Sprint(entry.ID), entry.TaskID, entry.Branch, entry.BaseBranch)

	message := fmt.Sprintf("Merge %s: task %s", entry.Branch, entry.TaskID)
	mergeSHA, err := git.MergeNoFF(".", entry.Branch, entry.BaseBranch, message)
	if err != nil {
		// Check for conflict.
		if conflictErr, ok := err.(*git.ConflictError); ok {
			git.AbortMerge(".")
			if dbErr := db.ConflictMerge(pool, entry.ID, conflictErr.Files); dbErr != nil {
				return fmt.Errorf("recording conflict: %w", dbErr)
			}
			// Add observation to the task about the conflict.
			db.AddObservation(pool, entry.TaskID, "merge-queue",
				fmt.Sprintf("Merge conflict on branch %s: %s", entry.Branch, conflictErr.Error()))
			fmt.Printf("  Conflict: %s\n", conflictErr.Error())
			return nil
		}

		// Other merge failure.
		if dbErr := db.FailMerge(pool, entry.ID, err.Error()); dbErr != nil {
			return fmt.Errorf("recording failure: %w", dbErr)
		}
		fmt.Printf("  Failed: %v\n", err)
		return nil
	}

	if err := db.CompleteMerge(pool, entry.ID, mergeSHA); err != nil {
		return fmt.Errorf("completing merge: %w", err)
	}

	// Optionally remove worktree after successful merge.
	if err := git.WorktreeRemove(".", entry.WorktreeDir); err != nil {
		fmt.Printf("  warning: could not remove worktree %s: %v\n", entry.WorktreeDir, err)
	}

	fmt.Printf("  Merged: %s\n", mergeSHA)
	return nil
}

func printMergeQueue() error {
	entries, err := db.ListMergeQueue(pool)
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		fmt.Println("Merge queue is empty.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tTASK\tBRANCH\tBASE\tSTATUS\tENQUEUED\n")
	for _, e := range entries {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
			e.ID, e.TaskID, e.Branch, e.BaseBranch, e.Status,
			relativeTime(e.EnqueuedAt))
	}
	w.Flush()
	return nil
}
