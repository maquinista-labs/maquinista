package main

import (
	"log"

	"github.com/spf13/cobra"
)

// Per plans/active/detached-processes.md, top-level `stop` (post-D.4)
// tears down the full stack: dashboard first (so the UI stops
// sending actions into the bot), then orchestrator (which finishes
// draining mailbox and takes tmux + DB cleanup with it). Each half
// is independent — a dead dashboard doesn't block the orchestrator
// shutdown.

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the full stack: dashboard then orchestrator",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := runDashboardStop(); err != nil {
			log.Printf("dashboard stop failed: %v", err)
		}
		return runOrchestratorStop()
	},
}
