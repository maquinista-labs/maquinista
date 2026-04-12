package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/maquinista-labs/maquinista/internal/scheduler"
	"github.com/spf13/cobra"
)

var schedulerCmd = &cobra.Command{
	Use:   "scheduler",
	Short: "Fire scheduled_jobs rows into agent_inbox",
	Long: `Single-replica loop over scheduled_jobs. Each tick, claims every due
row with FOR UPDATE SKIP LOCKED, enqueues an agent_inbox with from_kind='scheduled'
and idempotent external_msg_id='sched:<job_id>:<fire_ts>', then advances
next_run_at via robfig/cron in the job's timezone.`,
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
			log.Println("scheduler: shutdown requested")
			cancel()
		}()
		log.Println("scheduler: starting")
		return scheduler.Run(ctx, pool, scheduler.DefaultConfig())
	},
}

func init() {
	rootCmd.AddCommand(schedulerCmd)
}
