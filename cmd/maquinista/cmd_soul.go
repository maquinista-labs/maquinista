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

var (
	soulImportForce    bool
	soulTemplateID     string
	soulTemplateDefault bool
)

var soulImportCmd = &cobra.Command{
	Use:   "import <agent-id> <file.md>",
	Short: "Bulk-replace an agent's soul from a Markdown file (scans for prompt-injection)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulImport(args[0], args[1])
	},
}

var soulExportCmd = &cobra.Command{
	Use:   "export <agent-id>",
	Short: "Dump the raw soul fields as Markdown to stdout (round-trippable via soul import)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulExport(args[0])
	},
}

var soulTemplateCmd = &cobra.Command{
	Use:   "template",
	Short: "Manage soul templates (list / show / set-default)",
}

var soulTemplateListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all soul templates",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulTemplateList()
	},
}

var soulTemplateShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print one soul template",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulTemplateShow(args[0])
	},
}

var soulTemplateSetDefaultCmd = &cobra.Command{
	Use:   "set-default <id>",
	Short: "Mark one template is_default=TRUE (and clear the flag on others)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSoulTemplateSetDefault(args[0])
	},
}

func init() {
	soulRenderCmd.Flags().IntVar(&soulRenderMaxChars, "max-chars", 32000, "truncate rendered output to this many chars (0=no truncation)")
	soulImportCmd.Flags().BoolVar(&soulImportForce, "force", false, "proceed despite warn-severity prompt-injection findings")

	soulTemplateCmd.AddCommand(soulTemplateListCmd, soulTemplateShowCmd, soulTemplateSetDefaultCmd)
	soulCmd.AddCommand(soulRenderCmd, soulShowCmd, soulImportCmd, soulExportCmd, soulTemplateCmd)
	rootCmd.AddCommand(soulCmd)
}

func runSoulImport(agentID, path string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	findings := soul.ScanForInjection(string(body))
	if soul.HasBlockingFindings(findings) {
		printFindings(findings)
		return fmt.Errorf("blocked by prompt-injection scanner (severity=block); refusing to import")
	}
	if len(findings) > 0 && !soulImportForce {
		printFindings(findings)
		return fmt.Errorf("prompt-injection warnings (severity=warn); pass --force to proceed")
	}

	parsed := parseSoulMarkdown(string(body))
	// Verify the agent exists first.
	var exists int
	if err := pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE id=$1`, agentID).Scan(&exists); err != nil {
		return fmt.Errorf("no such agent: %s", agentID)
	}
	// Preserve existing template_id + allow_delegation + max_iter.
	cur, _ := soul.Load(ctx, pool, agentID)
	if cur != nil {
		parsed.TemplateID = cur.TemplateID
		parsed.AllowDelegation = cur.AllowDelegation
		parsed.MaxIter = cur.MaxIter
		parsed.RespectContext = cur.RespectContext
	}
	parsed.AgentID = agentID
	if err := soul.Upsert(ctx, pool, parsed); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	fmt.Printf("Imported soul for %s (%d bytes).\n", agentID, len(body))
	return nil
}

func runSoulExport(agentID string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, err := soul.Load(ctx, pool, agentID)
	if err != nil {
		if errors.Is(err, soul.ErrNotFound) {
			return fmt.Errorf("no soul for agent %s", agentID)
		}
		return err
	}
	fmt.Print(renderSoulMarkdown(*s))
	return nil
}

func runSoulTemplateList() error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
		SELECT id, name, role, is_default FROM soul_templates ORDER BY id
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, role string
		var isDef bool
		if err := rows.Scan(&id, &name, &role, &isDef); err != nil {
			return err
		}
		marker := " "
		if isDef {
			marker = "*"
		}
		fmt.Printf("%s %-20s %-25s %s\n", marker, id, name, role)
	}
	return rows.Err()
}

func runSoulTemplateShow(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tpl, err := soul.LoadTemplate(ctx, pool, id)
	if err != nil {
		if errors.Is(err, soul.ErrNotFound) {
			return fmt.Errorf("no template: %s", id)
		}
		return err
	}
	// Render via the Soul template (close enough for human viewing).
	fmt.Print(soul.Render(soul.Soul{
		Name: tpl.Name, Tagline: tpl.Tagline, Role: tpl.Role, Goal: tpl.Goal,
		CoreTruths: tpl.CoreTruths, Boundaries: tpl.Boundaries, Vibe: tpl.Vibe,
		Continuity: tpl.Continuity, Extras: tpl.Extras,
	}, 0))
	return nil
}

func runSoulTemplateSetDefault(id string) error {
	if err := connectDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE soul_templates SET is_default=FALSE WHERE is_default=TRUE`); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE soul_templates SET is_default=TRUE WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no template: %s", id)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("Default template → %s\n", id)
	return nil
}

// printFindings writes scan results to stderr in a compact tabular form.
func printFindings(findings []soul.Finding) {
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "  [%s] offset=%d pattern=%q\n    %s\n",
			f.Severity, f.Offset, f.Pattern, f.Excerpt)
	}
}

// parseSoulMarkdown converts a markdown export back into a Soul struct.
// Recognized headings: top-level '# <Name>' with optional tagline on the
// next non-empty line, and '## Section' for each known field. Any other
// '## X' section goes into Extras[X]. Mirrors the round-trip contract
// in plans/active/agent-soul-db-state.md §Field mapping.
func parseSoulMarkdown(md string) soul.Soul {
	s := soul.Soul{Extras: map[string]string{}, MaxIter: 25, RespectContext: true, Version: 1}
	lines := strings.Split(md, "\n")

	// Parse top heading.
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "# ") {
		head := strings.TrimSpace(strings.TrimPrefix(lines[i], "# "))
		// "You are Alice, a Reviewer." → split on ", a "
		head = strings.TrimSuffix(head, ".")
		if strings.HasPrefix(head, "You are ") {
			head = strings.TrimPrefix(head, "You are ")
		}
		if idx := strings.LastIndex(head, ", a "); idx >= 0 {
			s.Name = strings.TrimSpace(head[:idx])
			s.Role = strings.TrimSpace(head[idx+len(", a "):])
		} else {
			s.Name = head
		}
		i++
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i < len(lines) && strings.HasPrefix(lines[i], "> ") {
			s.Tagline = strings.TrimSpace(strings.TrimPrefix(lines[i], "> "))
			i++
		}
	}

	// Walk ## sections.
	currentHeader := ""
	var currentBody []string
	flush := func() {
		body := strings.TrimSpace(strings.Join(currentBody, "\n"))
		switch strings.ToLower(currentHeader) {
		case "":
		case "core truths":
			s.CoreTruths = body
		case "boundaries":
			s.Boundaries = body
		case "vibe":
			s.Vibe = body
		case "continuity":
			s.Continuity = body
		case "your goal", "goal":
			s.Goal = body
		default:
			if currentHeader != "" {
				s.Extras[currentHeader] = body
			}
		}
		currentBody = nil
	}

	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "## ") {
			flush()
			currentHeader = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		// "**Your goal:** …" — single-line goal form.
		if strings.HasPrefix(strings.TrimSpace(line), "**Your goal:**") {
			s.Goal = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "**Your goal:**"))
			continue
		}
		currentBody = append(currentBody, line)
	}
	flush()
	return s
}

// renderSoulMarkdown dumps the soul's raw field contents as Markdown
// suitable for `soul import` to re-parse without loss. Differs from
// soul.Render() in that it emits the inert form (no LLM framing) so
// round-tripping is lossless.
func renderSoulMarkdown(s soul.Soul) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# You are %s, a %s.\n", s.Name, s.Role)
	if s.Tagline != "" {
		fmt.Fprintf(&b, "> %s\n", s.Tagline)
	}
	fmt.Fprintf(&b, "\n**Your goal:** %s\n\n", s.Goal)
	if s.CoreTruths != "" {
		fmt.Fprintf(&b, "## Core truths\n\n%s\n\n", s.CoreTruths)
	}
	if s.Boundaries != "" {
		fmt.Fprintf(&b, "## Boundaries\n\n%s\n\n", s.Boundaries)
	}
	if s.Vibe != "" {
		fmt.Fprintf(&b, "## Vibe\n\n%s\n\n", s.Vibe)
	}
	if s.Continuity != "" {
		fmt.Fprintf(&b, "## Continuity\n\n%s\n\n", s.Continuity)
	}
	for k, v := range s.Extras {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", k, v)
	}
	return b.String()
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
