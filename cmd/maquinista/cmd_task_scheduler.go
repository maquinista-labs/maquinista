package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/maquinista-labs/maquinista/internal/git"
	"github.com/maquinista-labs/maquinista/internal/orchestrator"
	"github.com/maquinista-labs/maquinista/internal/taskscheduler"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var taskSchedulerCmd = &cobra.Command{
	Use:   "task-scheduler",
	Short: "Dispatch ready tasks to per-task implementor agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			log.Println("task-scheduler: shutdown")
			cancel()
		}()

		// Real spawner: mark the row live but defer the actual pty/sidecar
		// wiring to a future bot-startup integration (cmd start launches
		// sidecars). Until then, the task-scheduler subcommand is most
		// useful alongside an existing bot process.
		spawner := orchestrator.AgentSpawnerFunc(func(ctx context.Context, id, wd, role string) error {
			log.Printf("task-scheduler: spawned agent row %s (wd=%s role=%s) — pty wiring handled by bot", id, wd, role)
			return nil
		})
		cfg := taskscheduler.Config{
			EnsureAgent: func(ctx context.Context, role, taskID string) (string, error) {
				return orchestrator.EnsureAgent(ctx, orchestrator.EnsureAgentParams{
					Pool: pool, Spawner: spawner, Role: role, TaskID: taskID,
				})
			},
			// MAQ-38 AC 2: freeze-retire notes state whether a pane existed
			// for the retired id (name-based probe — panes are -n <agentID>).
			PaneExists: tmux.WindowNameExists,
			// ADR-0008: silent-success freeze retires consult the artifacts
			// question (open PR + branch current) before requeueing vs
			// straight-to-review. Conservative probe: git errors read as
			// "not up to date" and never misroute a half-done branch.
			BranchUpToDate: func(worktree string) bool {
				return git.BranchUpToDate(worktree, "main")
			},
		}
		return taskscheduler.Run(ctx, pool, cfg)
	},
}

func init() {
	rootCmd.AddCommand(taskSchedulerCmd)
}
