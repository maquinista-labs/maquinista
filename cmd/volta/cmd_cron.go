package main

import (
	"fmt"
	"log"
	"time"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
)

var cronCmd = &cobra.Command{
	Use:   "cron",
	Short: "Cron daemon for recurring schedules",
}

var cronTickCmd = &cobra.Command{
	Use:   "tick",
	Short: "Run the cron tick loop (long-running)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		log.Println("cron: starting tick loop (every 30s)")

		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

		for {
			schedules, err := db.GetDueSchedules(pool)
			if err != nil {
				log.Printf("cron: error fetching due schedules: %v", err)
			} else {
				for _, sched := range schedules {
					log.Printf("cron: instantiating schedule %q", sched.Name)

					ids, err := instantiateTemplate(sched.Template, sched.ProjectID)
					if err != nil {
						log.Printf("cron: error instantiating %q: %v", sched.Name, err)
						continue
					}

					// Compute next run.
					cronSched, err := parser.Parse(sched.Cron)
					if err != nil {
						log.Printf("cron: bad cron for %q: %v", sched.Name, err)
						continue
					}
					nextRun := cronSched.Next(time.Now())

					if err := db.UpdateScheduleAfterRun(pool, sched.Name, time.Now(), nextRun); err != nil {
						log.Printf("cron: error updating %q: %v", sched.Name, err)
					}

					fmt.Printf("cron: %q → %d tasks created (next: %s)\n", sched.Name, len(ids), nextRun.Format("15:04:05"))
				}
			}

			time.Sleep(30 * time.Second)
		}
	},
}

func init() {
	cronCmd.AddCommand(cronTickCmd)
	rootCmd.AddCommand(cronCmd)
}
