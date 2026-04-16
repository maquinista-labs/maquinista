# Agent Soul + DB state proposal

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**. No markdown files, no JSON on disk, no dotfiles for persistent state.

## Context

openclaw gives every workspace a `SOUL.md` file that is injected into the
agent's system context at the start of every session
(`~/.openclaw/workspace/SOUL.md`, loaded before the first turn, truncated
to `agents.defaults.bootstrapMaxChars` = 20 KB). Its sections are
freeform Markdown but follow a template:

- **Core Truths** — non-negotiable identity statements
- **Boundaries** — hard restrictions (privacy, external caution…)
- **Vibe** — tone / personality
- **Continuity** — note that files persist identity across sessions

See `/openclaw/docs/reference/templates/SOUL.md` (lines 14–45) and the
worked example `SOUL.dev.md` ("C-3PO") lines 9–77. The soul is
workspace-scoped, not per-agent — all openclaw agents in one workspace
share one soul.

Maquinista already has per-agent `agent_settings.persona` +
`agent_settings.system_prompt` (migration 009). That covers the raw
text, but has three gaps:

1. **No structure.** It's one opaque TEXT blob. We can't render a
   coherent identity UI or let operators edit a single section.
2. **Not created at `agent add` time.** `multi-agent-registry.md`
   Phase 3 adds `--system-prompt FILE` + `--persona NAME` flags, but
   the row is only created if the operator remembers to pass them. A
   newly-added agent with no soul boots with an empty system prompt.
3. **No template / lineage.** If we want a "default maquinista soul"
   that every new agent inherits (then edits), there's nowhere to
   store it.

This plan treats the soul as a **first-class DB entity** populated at
agent creation, versioned, and composed at spawn time into the single
system-prompt string that the runner already consumes via
`PlannerCommand`.

## Scope

Four phases. Phases 1–2 are load-bearing; 3–4 are polish.

### Phase 1 — `agent_souls` table + seed

New table, one row per agent, owns the structured soul fields. Kept
separate from `agent_settings` so future columns (embedding, version,
revision history) don't muddy the settings table.

```sql
-- migration 014_agent_souls.sql
CREATE TABLE agent_souls (
  agent_id         TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  template_id      TEXT REFERENCES soul_templates(id),
  -- Identity (hybrid: maquinista's structured sections + CrewAI's role/goal).
  name             TEXT NOT NULL,         -- displayed identity ("Maquinista", "Alice")
  tagline          TEXT,                  -- one-liner shown in UI / agent list
  role             TEXT NOT NULL,         -- functional title ("Planner", "Reviewer")
  goal             TEXT NOT NULL,         -- imperative objective ("Ship PRs that land green")
  core_truths      TEXT NOT NULL DEFAULT '',-- bullet-list Markdown (what they *are*)
  boundaries       TEXT NOT NULL DEFAULT '',-- bullet-list Markdown (hard limits)
  vibe             TEXT NOT NULL DEFAULT '',-- tone / personality paragraph
  continuity       TEXT NOT NULL DEFAULT '',-- optional persistence note
  extras           JSONB NOT NULL DEFAULT '{}'::jsonb, -- escape hatch for custom sections
  -- Execution policy (adopted from CrewAI's Agent fields).
  allow_delegation BOOLEAN NOT NULL DEFAULT FALSE,  -- can this agent spawn sub-agents?
  max_iter         INTEGER NOT NULL DEFAULT 25,     -- iteration cap per turn (CrewAI default)
  respect_context  BOOLEAN NOT NULL DEFAULT TRUE,   -- summarize if near token limit
  version          INTEGER NOT NULL DEFAULT 1,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE soul_templates (
  id               TEXT PRIMARY KEY,      -- "default", "helper", "planner", …
  name             TEXT NOT NULL,
  tagline          TEXT,
  role             TEXT NOT NULL,
  goal             TEXT NOT NULL,
  core_truths      TEXT NOT NULL DEFAULT '',
  boundaries       TEXT NOT NULL DEFAULT '',
  vibe             TEXT NOT NULL DEFAULT '',
  continuity       TEXT NOT NULL DEFAULT '',
  extras           JSONB NOT NULL DEFAULT '{}'::jsonb,
  allow_delegation BOOLEAN NOT NULL DEFAULT FALSE,
  max_iter         INTEGER NOT NULL DEFAULT 25,
  is_default       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX soul_templates_one_default
  ON soul_templates ((is_default)) WHERE is_default;

-- Seed one default template so bootstrap works on empty install.
INSERT INTO soul_templates
  (id, name, tagline, role, goal, core_truths, boundaries, vibe, continuity,
   allow_delegation, max_iter, is_default)
VALUES ('default', 'Maquinista', 'Operator-in-the-loop engineering agent',
  'Engineering collaborator',
  'Ship small, correct changes that move the repo forward without surprising the operator.',
  '- Genuinely helpful. Ship small, correct changes.\n- Have opinions. Earn trust by being specific.\n- Never delegate understanding.',
  '- Do not run destructive git commands without confirmation.\n- Validate only at system boundaries.\n- Refuse to bypass CI/hooks.',
  'Calm, concise, terminal-native. Short sentences, no filler.',
  'State persists in Postgres between sessions; memory continuity lives in agent_memory.',
  FALSE, 25, TRUE);
```

**Why `role` + `goal` on top of structured sections**
(validated against `/home/otavio/code/crewAI/lib/crewai/src/crewai/agents/agent_builder/base_agent.py:162-338`):

CrewAI makes `role` + `goal` + `backstory` the mandatory trio and
they carry load-bearing semantics in every example:

- **`role`** — functional title. Three words. "Research Analyst",
  "Code Reviewer", "Data Cleaner". Shapes how the LLM frames its
  reasoning.
- **`goal`** — imperative objective, one sentence. "Produce a
  market-sizing brief that hedge funds can act on." The agent
  self-references this during multi-step reasoning.
- **`backstory`** — narrative context (the role-playing anchor).
  Maquinista's `core_truths` + `vibe` + `continuity` cover this
  together, and with more structure.

CrewAI's strength here is that `role` and `goal` are **not** part of
a free-text blob — they're separable, editable, templatable fields.
That's worth stealing. Maquinista keeps its structured sections on
top (CrewAI has no equivalent of `boundaries`, and burying hard
limits in backstory is a proven failure mode).

**`allow_delegation` + `max_iter`** — both are CrewAI Agent fields
(`base_agent.py:172,176`) that maquinista's soul plan didn't have.
`allow_delegation` gates access to the sub-agent spawning tool (see
`agent-to-agent-communication.md` Phase 4 — child agents now read
this column before exposing `spawn_subagent` in their tool manifest).
`max_iter` caps runaway tool-loops. Both shipped with sensible
defaults that operators rarely tune.

`agent_settings.system_prompt` stays — Phase 2 repurposes it as the
**derived artefact** (composed from the soul) rather than the source of
truth. Until then, both coexist: if `agent_souls` row is missing, fall
back to `agent_settings.system_prompt`.

### Phase 2 — Auto-create soul at agent creation time

Every code path that inserts into `agents` gets a matching
`agent_souls` row:

1. `db.RegisterAgent` (`internal/db/queries.go:605-620`) —
   **persistent agents only** (`role='user' AND task_id IS NULL`).
   Task-scoped agents from `internal/orchestrator/ensure_agent.go`
   do **not** get a soul (they're ephemeral worktree workers; their
   identity is the task).
2. `ensureDefaultAgent` / the Phase-1 reconcile helper in
   `multi-agent-registry.md` — on bootstrap, clones the default
   template into the new soul row.
3. `maquinista agent add <id>` CLI (`multi-agent-registry.md` Phase 3)
   — gains `--soul-template <id>` flag (default `"default"`), or
   `--soul-file <path>` to import from a Markdown file.

Creation helper:

```go
// internal/agent/soul.go
func CreateSoulFromTemplate(ctx context.Context, tx pgx.Tx,
    agentID, templateID string, overrides SoulOverrides) error
// Reads soul_templates row, applies overrides, inserts agent_souls row.
// templateID="" picks the default template.
```

`SoulOverrides` carries any CLI flags that want to replace individual
sections (`--name "Alice"`, `--vibe-file vibe.md`).

Behavior:

- `RegisterAgent` calls `CreateSoulFromTemplate` in the same transaction
  so a failure rolls back the whole agent creation.
- If the default template is missing (operator deleted it), creation
  falls back to an empty-string soul and logs a warning. The agent still
  boots, just with no identity — same as today.

### Phase 3 — Compose soul into the spawn system prompt

At spawn time, `cmd_start_reconcile.go` (per
`multi-agent-registry.md` Phase 2) renders a **layered** system prompt
(pattern validated against hermes-agent `prompt_builder.py:92-398` and
`run_agent.py:3332-3473`, which composes identity + tool-aware
guidance + model-specific guidance + memory + env/platform hints):

```go
// internal/agent/prompt.go
type PromptLayers struct {
  Soul          Soul                 // from agent_souls
  ToolGuidance  []string             // conditional blocks per enabled tool
  ModelGuidance string               // per-model-family discipline block
  Memory        string               // from memory.FetchForInjection (see agent-memory-db.md)
  Env           EnvHints             // platform, cwd, date
}
func Compose(layers PromptLayers, maxChars int) string
```

Layer order (matches hermes' proven ordering):

1. **Soul** — identity, vibe, boundaries (this plan). Rendered
   opening line mirrors CrewAI's `role_playing` slice
   (`translations/en.json`, verbatim: `"You are {role}. {backstory}\nYour
   personal goal is: {goal}"`) because every CrewAI example confirms
   this opener anchors the LLM's framing:

   ```
   # You are {{.Name}}, a {{.Role}}.
   > {{.Tagline}}

   **Your goal:** {{.Goal}}

   ## Core truths
   {{.CoreTruths}}

   ## Boundaries
   {{.Boundaries}}

   ## Vibe
   {{.Vibe}}

   {{ with .Continuity }}## Continuity
   {{ . }}{{ end }}

   {{ range $key, $val := .Extras }}## {{ $key }}
   {{ $val }}
   {{ end }}
   ```

2. **Tool-aware guidance** — injected conditionally per tool enabled for
   this agent (e.g. memory tool → memory-usage block; a2a tool →
   peer-discovery block). Keeps irrelevant instructions out.
3. **Model-family guidance** — a small block keyed on
   `agents.runner_config->>'model'` family (`gpt`, `claude`, `gemini`,
   `gemma`, `grok`, `codex`). Hermes calls these `TOOL_USE_ENFORCEMENT`
   / `GOOGLE_MODEL_OPERATIONAL_GUIDANCE`; for maquinista the only ones
   that matter today are a terse claude-code-style block (default) and
   a longer "absolute paths, verify-before-edit" block for gpt/gemini.
4. **Memory** — per `agent-memory-db.md` Phase 3 (pinned + recent).
5. **Env hints** — today's date, platform (telegram/discord/cron/…),
   worktree path, branch. Frozen at compose time for prefix-cache
   stability.

Composition is rendered into `$MAQUINISTA_DIR/prompts/<agent>.md` at
every reconcile tick. **The file is frozen for the duration of the
tmux process** — not rewritten mid-session — so the claude runner
keeps its prefix cache warm across turns. `maquinista soul edit`
takes effect on next respawn, not live. (Pattern: hermes caches at
`self._cached_system_prompt = …`, rebuilds only on compaction
[`run_agent.py:3332`]; openclaw config reload triggers process
restart via `run-loop.ts:34-112`.)

**Size budget and truncation.** Openclaw enforces `maxChars=12000` per
file and `60000` total, with **head+tail preservation** (70 % head,
20 % tail, a truncation marker in the middle —
`bootstrap.ts:86-144`). Steal the same shape:

```go
// internal/agent/prompt.go
const (
  SoulMaxChars      = 12_000  // per-field across all soul sections combined
  PromptTotalMax    = 32_000  // rendered composition hard cap
  HeadRatio, TailRatio = 0.70, 0.20
)
// Truncate(s, max) = s[0:head] + "\n\n…[truncated N chars]…\n\n" + s[len-tail:]
```

Applied per-layer then checked against `PromptTotalMax`; on overflow,
shrink Memory first, then Extras, then Vibe, then Boundaries. Core
Truths + identity header never truncated.

**Fallback chain.** `agent_settings.system_prompt` is deprecated but
kept for one release: if `agent_souls` row exists, it wins; else
legacy path reads `agent_settings.system_prompt`. A follow-up
migration drops the column after the deprecation window.

### Phase 4 — `maquinista soul …` CLI + import safety

```
maquinista soul show <agent-id>                  Render current soul to stdout
maquinista soul edit <agent-id> [--section NAME] Open in $EDITOR, save on close
maquinista soul import <agent-id> <file.md>      Bulk-replace from Markdown
maquinista soul export <agent-id> > file.md      Dump rendered soul
maquinista soul template list|show|set-default   Manage soul_templates rows
```

`edit` without `--section` opens the full rendered soul in one buffer,
round-trips back into structured fields by parsing the `## Section`
headers. With `--section vibe` it only edits that field's text.
Failing to parse on save prints the diff and aborts — never overwrites.

**Prompt-injection scanning on `soul import`.** Hermes scans any
file-sourced identity content for adversarial patterns before
accepting it (`prompt_builder.py:36-73` — catches "ignore previous",
"system prompt override", invisible Unicode, curl-based exfiltration).
Soul files are high-privilege — they literally rewrite the agent's
identity — so `soul import` and `soul edit` run the same scanner:

```go
// internal/agent/sanitize.go
func ScanForInjection(body string) []Finding
// Returns (pattern, offset, severity) tuples for:
//   - classic prompt-override phrases (case-insensitive regex set)
//   - invisible / bidi Unicode (\u202e, \u200b, zero-width joiners)
//   - exfil-shaped shell commands in code fences (curl|wget → external)
//   - excessive repeated whitespace (> 10k chars to force truncation)
```

On severity ≥ `warn`: print findings, require `--force` to proceed. On
severity = `block`: abort. The scanner also runs on `soul_templates`
inserts (operators sharing templates via git can be a vector).

## Field mapping: openclaw → maquinista

| openclaw SOUL.md section | agent_souls column | Notes |
|---|---|---|
| `# <Name>` heading | `name` | Required, NOT NULL |
| First line under heading | `tagline` | Optional one-liner |
| "## Core Truths" body | `core_truths` | Bullet Markdown |
| "## Boundaries" body | `boundaries` | Bullet Markdown |
| "## Vibe" body | `vibe` | Paragraph |
| "## Continuity" body | `continuity` | Optional |
| Any other `## X` section | `extras['X']` | JSONB escape hatch |

Import (`soul import <file>`) parses headings, stuffs unknown sections
into `extras`, and preserves order via a reserved `extras._order` JSONB
array.

## Interaction with other plans

- **`multi-agent-registry.md` Phase 2** — was going to read
  `agent_settings.system_prompt` into the tempfile. Switch to reading
  `agent_souls` via `Render()` instead; fallback chain handles the
  deprecation.
- **`multi-agent-registry.md` Phase 3** — `agent add` learns
  `--soul-template` / `--soul-file`. `agent edit` delegates soul
  edits to `soul edit`.
- **`agent-memory-db.md`** — soul and memory are composed separately
  at spawn; soul is fixed identity, memory is mutable context. The
  rendered system prompt concatenates them in order:
  soul → memory summary → per-turn context.
- **`json-state-migration.md`** — unrelated; no conflict.

## Files

New:

- `internal/db/migrations/014_agent_souls.sql`
- `internal/agent/soul.go` — `Soul`, `SoulOverrides`, `CreateSoulFromTemplate`, `Render`, `LoadForAgent`.
- `cmd/maquinista/cmd_soul.go` — cobra group.

Modified:

- `internal/db/queries.go` — `RegisterAgent` calls `CreateSoulFromTemplate`.
- `cmd/maquinista/cmd_start_reconcile.go` (from registry Phase 2) —
  reads soul instead of `agent_settings.system_prompt`.
- `cmd/maquinista/cmd_agent.go` (from registry Phase 3) —
  `--soul-template` / `--soul-file` flags on `agent add`.

## Verification per phase

- **Phase 1** — migration applies on a fresh DB; `SELECT * FROM
  soul_templates WHERE is_default;` returns one row.
- **Phase 2** — `DELETE FROM agents; DELETE FROM agent_souls;` +
  `./maquinista start` → one `agents` row + one `agent_souls` row, the
  latter cloned from the default template.
- **Phase 3** — `maquinista soul edit maquinista --section vibe` →
  modify vibe → `./maquinista start` → tmux pane starts `claude
  --system-prompt "$(cat …)"` and the file contains the new vibe.
  Telegram reply reflects the new tone.
- **Phase 4** — `maquinista soul import alice alice-soul.md` then
  `soul export alice` round-trips losslessly, including unknown
  sections landing in `extras` and re-rendering in the original order.

## Open questions

1. **Versioning.** The schema includes `version` but nothing bumps it
   yet. Do we want `agent_soul_revisions(agent_id, version, …, changed_by,
   changed_at)` for audit? Defer unless soul edits start causing drama.
2. **Templates as Markdown files on disk vs DB rows.** DB wins for
   multi-host setups; file wins for version control. This plan picks
   DB; operators who want git-versioned souls use
   `soul export | git commit`.
3. **Soul size cap.** Resolved in Phase 3: soft cap + head/tail
   truncation at compose time (openclaw pattern, 12 KB per section,
   32 KB total prompt). No DB-level CHECK — operators can write long
   drafts, `Compose()` shrinks at render time.
4. **Task-scoped agents.** Should a task-agent inherit its parent
   user-agent's soul? Current plan says no (task agents have no
   identity). Revisit if task agents start speaking directly to users.
5. **Model-family guidance source.** In Phase 3 layer 3, where do the
   per-model blocks live? Hermes hard-codes them (constants in
   `prompt_builder.py`); openclaw config-drives them. Start with Go
   constants keyed by family prefix (`gpt_`, `claude_`, `gemini_`,
   `gemma_`, `grok_`, `codex_`); promote to a `model_guidance` DB
   table only if operators start wanting to edit them.
