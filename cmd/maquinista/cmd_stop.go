package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Per plans/active/detached-processes.md, the tmux + DB-agent
// cleanup used to live here; it moved to runOrchestratorStop (see
// cmd_orchestrator.go) where it sits alongside the daemon it cleans
// up after. This top-level `stop` is a deprecation shim that calls
// through; D.4 replaces it with a "stop both daemons" bootstrap.

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Maquinista orchestrator (deprecated alias for `orchestrator stop`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(os.Stderr, "warning: `maquinista stop` is a deprecated alias; use `maquinista orchestrator stop` (D.4 will repurpose `maquinista stop` for the full stack).")
		return runOrchestratorStop()
	},
}
