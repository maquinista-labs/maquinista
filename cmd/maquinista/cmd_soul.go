package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maquinista-labs/maquinista/internal/soul"
	"github.com/spf13/cobra"
)

// `maquinista soul` — CLI for the agent-identity DB surface. Kept
// minimal: read-side support so the runner spawn flow can pull the
// rendered system prompt directly from Postgres (no prompts/<id>.md
// file on disk). Write-side CLI (soul edit / import / export) is a
// follow-up.

var soulCmd = &cobra.Command{
	Use:   "soul",
	Short: "Inspect and render per-agent souls",
}

var soulRenderMaxChars int

var soulRenderCmd = &cobra.Command{
	Use:   "render <agent-id>",
	Short: "Print the rendered system prompt for an agent to stdout",
	Long: `Render the agent's soul as a system prompt and print to stdout.

Spawn scripts use this instead of a prompts/<id>.md file on disk so the
DB stays the single source of truth (per §0 of maquinista-v2.md). A
missing row returns an empty body + exit 0 so the caller can still
spawn the runner with an empty --system-prompt.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulRender(args[0])
	},
}

var soulShowCmd = &cobra.Command{
	Use:   "show <agent-id>",
	Short: "Print agent soul as human-readable Markdown (same as render today)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulRender(args[0])
	},
}

func init() {
	soulRenderCmd.Flags().IntVar(&soulRenderMaxChars, "max-chars", 32000, "truncate rendered output to this many chars (0=no truncation)")
	soulCmd.AddCommand(soulRenderCmd, soulShowCmd)
	rootCmd.AddCommand(soulCmd)
}

func runSoulRender(agentID string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, err := soul.Load(ctx, pool, agentID)
	if err != nil {
		if errors.Is(err, soul.ErrNotFound) {
			// No soul → empty output, exit 0. Caller's `$(cmd)` lands as
			// an empty string and the runner starts with no system prompt.
			return nil
		}
		return err
	}
	fmt.Print(soul.Render(*s, soulRenderMaxChars))
	return nil
}
