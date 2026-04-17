package soul

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/maquinista-labs/maquinista/internal/memory"
)

// PromptLayers are the composable ingredients of the system prompt the
// runner receives at spawn time. Ordering matches the hermes-agent
// proven sequence (identity → tool guidance → model family → memory →
// env hints). See plans/active/agent-soul-db-state.md §Phase 3.
type PromptLayers struct {
	Soul          Soul
	Blocks        []memory.Block    // core in-context blocks
	Archival      []memory.Memory   // pinned + long-term archival passages
	ToolGuidance  []string          // one entry per enabled tool
	ModelGuidance string            // per-model-family directive block
	Env           EnvHints
}

// EnvHints are the platform/workspace facts we always want the agent to
// know. Frozen at compose time so the prefix-cache stays warm across
// turns (the file content is stable until the next spawn).
type EnvHints struct {
	Platform string // "telegram" | "cron" | "webhook" | …
	CWD      string
	Branch   string
	Date     string // YYYY-MM-DD, populated from time.Now in Compose if empty
	GoOS     string // runtime.GOOS, populated by Compose if empty
}

// Compose assembles the layered system prompt. maxTotalChars is the hard
// cap on the final rendered output; 0 disables truncation. When the
// total grows past the cap, later layers shrink first (memory → extras →
// vibe → boundaries); soul identity + goal never get truncated.
//
// This is used by `maquinista soul render` + the spawner to produce the
// string that lands in `--system-prompt "$(…)"`.
func Compose(layers PromptLayers, maxTotalChars int) string {
	var b strings.Builder

	// ---- 1. Soul identity + goal (never truncated). -------------------
	b.WriteString(Render(layers.Soul, SoulMaxChars))
	if !strings.HasSuffix(b.String(), "\n\n") {
		b.WriteString("\n\n")
	}

	// ---- 2. Tool-aware guidance (optional). ---------------------------
	if len(layers.ToolGuidance) > 0 {
		b.WriteString("## Tool guidance\n\n")
		for _, g := range layers.ToolGuidance {
			gg := strings.TrimSpace(g)
			if gg == "" {
				continue
			}
			b.WriteString(gg)
			b.WriteString("\n\n")
		}
	}

	// ---- 3. Model-family guidance (optional). -------------------------
	if mg := strings.TrimSpace(layers.ModelGuidance); mg != "" {
		b.WriteString("## Model guidance\n\n")
		b.WriteString(mg)
		b.WriteString("\n\n")
	}

	// ---- 4. Memory: core blocks + archival pins + recent long-term. ---
	if len(layers.Blocks) > 0 {
		b.WriteString("## Core memory\n\n")
		b.WriteString(renderBlocks(layers.Blocks))
		b.WriteString("\n")
	}
	if len(layers.Archival) > 0 {
		b.WriteString("## Relevant memories\n\n")
		b.WriteString(renderArchival(layers.Archival))
		b.WriteString("\n")
	}

	// ---- 5. Env hints. ------------------------------------------------
	b.WriteString(renderEnv(layers.Env))

	rendered := strings.TrimRight(b.String(), "\n") + "\n"
	if maxTotalChars > 0 && len(rendered) > maxTotalChars {
		// Not ideal but non-lossy enough: preserve head (soul + guidance)
		// and tail (env hints). Same head/tail pattern as Render's.
		return truncateHeadTail(rendered, maxTotalChars)
	}
	return rendered
}

// SoulMaxChars caps the soul section so a runaway core_truths / vibe
// can't starve the downstream layers. Mirrors openclaw's bootstrapMax.
const SoulMaxChars = 12000

// ComposeForAgent is the one-call happy path: pull soul + memory from
// Postgres for agentID, stitch everything, return the rendered string.
// env.Date / env.GoOS are auto-filled when left empty.
func ComposeForAgent(ctx context.Context, q Querier, mq memory.Querier, agentID string, env EnvHints, modelGuidance string, toolGuidance []string, maxTotalChars int) (string, error) {
	s, err := Load(ctx, q, agentID)
	if err != nil {
		if err == ErrNotFound {
			return "", nil // empty soul → empty prompt
		}
		return "", err
	}

	blocks, err := memory.LoadAllBlocks(ctx, mq, agentID)
	if err != nil {
		return "", fmt.Errorf("load blocks: %w", err)
	}

	archival, err := memory.FetchForInjection(ctx, mq, agentID, 10)
	if err != nil {
		return "", fmt.Errorf("fetch archival: %w", err)
	}

	if env.Date == "" {
		env.Date = time.Now().UTC().Format("2006-01-02")
	}
	if env.GoOS == "" {
		env.GoOS = runtime.GOOS
	}

	layers := PromptLayers{
		Soul:          *s,
		Blocks:        blocks,
		Archival:      archival,
		ToolGuidance:  toolGuidance,
		ModelGuidance: modelGuidance,
		Env:           env,
	}
	return Compose(layers, maxTotalChars), nil
}

func renderBlocks(blocks []memory.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if strings.TrimSpace(blk.Value) == "" {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", blk.Label, strings.TrimSpace(blk.Value))
	}
	return b.String()
}

func renderArchival(mems []memory.Memory) string {
	var b strings.Builder
	for _, m := range mems {
		tag := ""
		if m.Pinned {
			tag = " _(pinned)_"
		}
		fmt.Fprintf(&b, "- **%s**%s — %s\n", m.Title, tag, oneLine(m.Body))
	}
	return b.String()
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 240 {
		s = s[:237] + "…"
	}
	return s
}

func renderEnv(env EnvHints) string {
	var b strings.Builder
	b.WriteString("## Environment\n\n")
	if env.Platform != "" {
		fmt.Fprintf(&b, "- Platform: %s\n", env.Platform)
	}
	if env.CWD != "" {
		fmt.Fprintf(&b, "- Working directory: %s\n", env.CWD)
	}
	if env.Branch != "" {
		fmt.Fprintf(&b, "- Git branch: %s\n", env.Branch)
	}
	if env.Date != "" {
		fmt.Fprintf(&b, "- Today: %s\n", env.Date)
	}
	if env.GoOS != "" {
		fmt.Fprintf(&b, "- OS: %s\n", env.GoOS)
	}
	return b.String()
}
