package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Resolve runner_type → model-family guidance block.
	var runnerType string
	_ = pool.QueryRow(ctx,
		`SELECT COALESCE(runner_type,'') FROM agents WHERE id=$1`, agentID,
	).Scan(&runnerType)

	env := soul.EnvHints{
		Platform: os.Getenv("MAQUINISTA_PLATFORM"),
		CWD:      lookupAgentCWD(ctx, agentID),
	}

	composed, err := soul.ComposeForAgent(
		ctx, pool, pool, agentID,
		env, modelFamilyGuidance(runnerType), nil, soulRenderMaxChars,
	)
	if err != nil {
		if errors.Is(err, soul.ErrNotFound) {
			return nil
		}
		return err
	}
	fmt.Print(composed)
	return nil
}

// modelFamilyGuidance returns a terse per-model-family directive block.
// Hermes calls these TOOL_USE_ENFORCEMENT / GOOGLE_MODEL_OPERATIONAL_GUIDANCE;
// for maquinista we only ship two variants: the claude default and a
// stricter "verify-before-edit" block for other runners. Extend by
// adding more cases as runners come online.
func modelFamilyGuidance(runnerType string) string {
	switch runnerType {
	case "claude", "openclaude":
		return "" // Claude's built-in system prompt already covers tool-use discipline.
	case "opencode":
		return strings.TrimSpace(`
When using tools: always prefer absolute paths, verify the target file exists
before editing (read_file / stat first), and cite the final file:line when
reporting changes. Never trust unverified diff context from earlier turns.
`)
	}
	return ""
}

func lookupAgentCWD(ctx context.Context, agentID string) string {
	var cwd string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(cwd,'') FROM agents WHERE id=$1`, agentID).Scan(&cwd)
	return cwd
}
