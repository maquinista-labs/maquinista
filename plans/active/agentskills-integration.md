# agentskills.io Integration

Integrate the [agentskills.io](https://agentskills.io) open standard so operators can install
community skills from any compatible registry and assign them to individual agents. Skills are
SKILL.md directories — the same format Claude Code, Cursor, OpenCode, and 30+ other tools already
use — so the same skill works across the whole ecosystem.

This plan incorporates concrete implementation patterns from two reference codebases:
- **OpenClaw** (`../openclaw`) — `src/agents/skills/workspace.ts`, `src/agents/skills/agent-filter.ts`
- **Hermes Agent** (`../hermes-agent`) — `agent/prompt_builder.py`, `tools/skills_hub.py`, `tools/skills_guard.py`

---

## What a skill is

```
skill-name/
├── SKILL.md          # required: YAML frontmatter + markdown instructions
├── scripts/          # optional: executable code the agent can run
├── references/       # optional: supplemental docs loaded on demand
└── assets/           # optional: templates, data files
```

`SKILL.md` frontmatter (per agentskills.io spec):

```yaml
---
name: code-review
description: Perform structured PR reviews with security and style checks. Use when…
license: MIT
compatibility: Requires gh CLI
metadata:
  author: someone
  version: "1.2.0"
---
```

---

## Storage

### Filesystem (source of truth for content)

Scan paths in precedence order (project-level overrides user-level for the same name).
Matches the agentskills.io spec convention plus cross-client interop paths:

| Priority | Scope | Path |
|---|---|---|
| 1 (highest) | Project / workspace | `<cwd>/.maquinista/skills/<name>/` |
| 2 | Project / cross-client | `<cwd>/.agents/skills/<name>/` |
| 3 | Project / Claude compat | `<cwd>/.claude/skills/<name>/` |
| 4 | User | `~/.maquinista/skills/<name>/` |
| 5 | User / cross-client | `~/.agents/skills/<name>/` |
| 6 (lowest) | User / Claude compat | `~/.claude/skills/<name>/` |

OpenClaw scans 6 source layers in the same precedence pattern
(`extra < bundled < managed < agents-skills-personal < agents-skills-project < workspace`).
Trust check: project-level skills only load if the working directory is trusted — prevents a
cloned repo from injecting skills silently.

Skip `node_modules/`, `.git/`, max scan depth 4. Log a warning on name collision.

### DB (assignment tracking only — no skill content in DB)

```sql
-- Which skills are active for which agent.
CREATE TABLE agent_skill_assignments (
    agent_id     TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_name   TEXT        NOT NULL,
    scope        TEXT        NOT NULL CHECK (scope IN ('user','project')),
    trust_level  TEXT        NOT NULL DEFAULT 'community'
                             CHECK (trust_level IN ('builtin','trusted','community')),
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    installed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (agent_id, skill_name)
);

-- Installation provenance and audit log (mirrors Hermes lock.json approach).
CREATE TABLE skill_installs (
    skill_name   TEXT        NOT NULL,
    source       TEXT        NOT NULL,   -- 'github' | 'clawhub' | 'agentskills' | 'local'
    identifier   TEXT        NOT NULL,   -- e.g. "openai/skills/code-review"
    trust_level  TEXT        NOT NULL,
    content_hash TEXT        NOT NULL,
    installed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (skill_name)
);
```

---

## Context budget management

This is the most critical design decision. Both reference codebases converge on the same strategy:
**progressive degradation** as budget fills up.

### OpenClaw's three-stage approach (adopt this)

From `src/agents/skills/workspace.ts:639–689`:

```
Stage 1 — Full format  (name + description + body, all skills)
  → if chars > maxSkillsPromptChars:
Stage 2 — Compact format  (name + install path only, no description, all skills)
  → if chars still > (maxSkillsPromptChars - 150):
Stage 3 — Binary search  (compact format, largest prefix that fits within budget)
  → truncated=true, warn the agent
```

This ensures the agent always sees *something* for every skill (even just a name) before skills
are dropped entirely, which is better than silently omitting them.

### Hard limits (all overridable per agent)

Matching OpenClaw's defaults from `workspace.ts:117–121`:

| Limit | Default | Notes |
|---|---|---|
| `max_skills_prompt_chars` | 18 000 chars | Total skill block in system prompt |
| `max_skills_in_prompt` | 150 | Count cap before budget check |
| `max_skill_file_bytes` | 256 000 bytes | Per-SKILL.md read cap |
| `max_candidates_per_root` | 300 | Scan cap per skills directory |

Per-agent override: `agent_skill_assignments` can carry a `max_skills_prompt_chars` column, or a
separate `agent_skill_limits` table. OpenClaw does this via
`config.agents.list[id].skillsLimits.maxSkillsPromptChars` (see `agent-filter.ts:38–51`).

**Default active skills per agent: 5.** Beyond 5 assigned skills the budget risk grows fast —
at 5 000 tokens each that's 25 000 tokens added to soul + memory. The count cap + char budget
together prevent runaway context.

### Path compaction (OpenClaw trick, `workspace.ts` path rendering)

Replace `$HOME` with `~` in skill paths before rendering. Saves ~5–6 tokens per skill. Small but
free.

### Disk snapshot cache (Hermes approach, `prompt_builder.py:607–689`)

Hermes keeps a `.skills_prompt_snapshot.json` in the skills directory — a pre-parsed metadata
index of all SKILL.md frontmatter, built on first scan and invalidated when any SKILL.md mtime
changes. Avoids re-parsing SKILL.md files on every agent spawn. Cache key includes
`(skills_dir, available_tools, platform_hint)` so per-platform filters produce distinct entries.

Implement as `~/.maquinista/skills/.snapshot.json`:
```json
{
  "version": 1,
  "generated_at": "2026-04-19T...",
  "skills": [
    { "name": "code-review", "description": "...", "path": "...", "mtime": 1234567890 }
  ]
}
```

Invalidate any entry whose `mtime` has changed. Rebuild missing entries. This makes agent spawns
with many skills installed nearly free.

---

## System prompt integration

Skills are a new layer in `soul/compose.go`'s `PromptLayers`, inserted after tool guidance and
before memory:

```go
type PromptLayers struct {
    Soul          Soul
    Blocks        []memory.Block
    Archival      []memory.Memory
    ToolGuidance  []string
    ModelGuidance string
    Skills        []SkillEntry   // NEW
    Env           EnvHints
}

type SkillEntry struct {
    Name        string
    Description string   // always present (Tier 1)
    Body        string   // full SKILL.md body, only for assigned skills (Tier 2)
    Path        string   // compact path for fallback rendering
}
```

`Compose()` renders a `## Skills` section using the three-stage degradation logic above. The
`maxTotalChars` cap already protects the whole prompt; the skills budget sits inside it.

`maquinista soul render <id>` already calls `ComposeForAgent` — no change to the spawn path.
The spawner loads assigned skills from `agent_skill_assignments` + reads from filesystem.

---

## Security: trust levels and scanning

Hermes (`tools/skills_guard.py`) enforces a three-tier install policy with static analysis:

| Trust level | Safe | Caution | Dangerous |
|---|---|---|---|
| `builtin` (ships with maquinista) | allow | allow | allow |
| `trusted` (`openai/skills`, `anthropics/skills`) | allow | allow | block |
| `community` | allow | block | block |

Scanner categories: exfiltration, prompt injection, destructive commands, persistence mechanisms,
unexpected network calls, obfuscation.

**Quarantine → scan → install flow** (matching Hermes `skills_hub.py`):
1. Download skill into `~/.maquinista/skills/.hub/quarantine/<name>/`
2. Static scan (regex patterns against all files)
3. If verdict allows: move to `~/.maquinista/skills/<name>/`, record in `skill_installs`
4. Append to `~/.maquinista/skills/.hub/audit.log`

Lock file at `~/.maquinista/skills/.hub/lock.json` tracks name → `{source, identifier, hash, installed_at}`.

---

## Multi-source marketplace

Hermes aggregates multiple sources in `tools/skills_hub.py`. We adopt the same model:

| Source | URL / mechanism | Trust |
|---|---|---|
| agentskills.io | `https://agentskills.io/registry/index.json` (TBD) | community |
| openai/skills | `github.com/openai/skills` via GitHub Contents API | trusted |
| anthropics/skills | `github.com/anthropics/skills` via GitHub Contents API | trusted |
| ClawHub | `https://clawhub.ai` REST API | community |
| GitHub (any) | `github.com/<owner>/<repo>` via Contents API | community |
| Local path | filesystem copy | builtin |

Search deduplicates by name, preferring higher trust levels (same as Hermes `skills_hub.py:45`).

Index cache TTL: 1 hour (matches Hermes `INDEX_CACHE_TTL = 3600`). Stored at
`~/.maquinista/skills/.hub/index-cache/<source>.json`.

GitHub auth: try `GITHUB_TOKEN` → `gh auth token` → unauthenticated (60 req/hr, public repos
only). Same priority chain as Hermes `GitHubAuth._resolve_token()`.

---

## CLI (`maquinista skill`)

```
maquinista skill install <owner/repo[/path]>  # from GitHub (trusted or community)
maquinista skill install --local <path>        # from local directory (builtin trust)
maquinista skill install --force <owner/repo>  # bypass caution/dangerous blocks (expert)
maquinista skill search <query>                # search all configured sources
maquinista skill list                          # list installed (with trust level + assignments)
maquinista skill info <name>                   # show frontmatter + scan result
maquinista skill assign <agent-id> <name>      # enable for specific agent
maquinista skill unassign <agent-id> <name>
maquinista skill remove <name>                 # delete from filesystem + all assignments
maquinista skill scan <name>                   # re-run security scan, print report
```

Install mechanism: GitHub Contents API download into quarantine → scan → install. No npm, no pip.
`git` not required.

---

## Telegram commands

Thin wrappers, usable from any topic:

```
/skill install <owner/repo>
/skill search <query>
/skill list
/skill assign <name>      # infers agent from current topic
/skill remove <name>
/skill info <name>        # shows description + trust level
```

`/skill assign` without an agent-id infers the current topic's agent (same pattern as all
other topic-scoped commands).

---

## Dashboard integration

**Principle: zero new nav items.** Skills surface in exactly two places.

### 1. Agent detail page — Skills tab

Add a **Skills** tab alongside Timeline/Inbox/Outbox/Workspaces. Shows:
- Installed skills available to assign (with one-click toggle, trust badge)
- Currently assigned skills with enable/disable toggle and a "View" sheet for SKILL.md content
- "Install new" input (GitHub URL or shorthand) → calls install API → scan result shown inline
- Budget indicator: `X / 18 000 chars used` so operator can see when they're close to the cap

### 2. Global `/skills` page (power-user, not in nav)

Only reachable via "Manage all skills →" from the agent detail Skills tab. Shows:
- All installed skills across user + project scope, with trust badges
- Which agents each is assigned to
- Bulk install / remove / re-scan

Not in main nav. Not in bottom nav.

### API endpoints

```
GET  /api/skills                              # list all installed (fs scan + snapshot)
GET  /api/skills/search?q=…&source=…         # proxy to registry search
POST /api/skills/install                      # body: { identifier, source, force? }
GET  /api/skills/:name                        # frontmatter + scan result + body preview
DEL  /api/skills/:name                        # remove + all assignments
GET  /api/agents/:id/skills                   # assigned skills for agent (with budget usage)
POST /api/agents/:id/skills/:name/assign
POST /api/agents/:id/skills/:name/unassign
```

---

## Implementation phases

### Phase 1 — Core (no UI changes)
1. DB migration: `agent_skill_assignments`, `skill_installs` tables
2. `internal/skill` package:
   - SKILL.md parser (YAML frontmatter + body)
   - Filesystem scanner with source-precedence ordering
   - Snapshot cache (`~/.maquinista/skills/.snapshot.json`)
   - Three-stage budget enforcer (full → compact → binary-search prefix)
3. `internal/skill/hub` package:
   - Multi-source registry client (agentskills.io, GitHub, ClawHub)
   - Index cache (1h TTL)
   - GitHub auth chain (env → gh CLI → anonymous)
4. `internal/skill/guard` package:
   - Static scanner (regex threat patterns: exfiltration, injection, destructive, persistence, network, obfuscation)
   - Quarantine → scan → install flow
   - Lock file + audit log
5. `maquinista skill` CLI subcommand (install, list, search, assign, unassign, remove, info, scan)
6. `PromptLayers.Skills []SkillEntry` + three-stage renderer in `soul/compose.go`
7. `ComposeForAgent` loads assigned skills at spawn time

### Phase 2 — Telegram
8. `/skill` bot command family
9. Topic-scoped agent inference for assign

### Phase 3 — Dashboard
10. API endpoints (skills scan, search proxy, install, assign/unassign)
11. Skills tab on agent detail (assign toggles, budget indicator, install input)
12. Global `/skills` page (linked from agent detail only)

---

## Risks and mitigations

**Context budget overflow**: Five skills at the 5 000-token spec max adds ~25 000 tokens on top
of soul + memory. Mitigated by: (a) three-stage degradation drops to compact/count before
anything is lost; (b) `maxTotalChars` in `Compose()` is the hard outer cap; (c) default
`max_skills_in_prompt=5` per agent until operator raises it.

**Stale snapshot**: SKILL.md content is read at spawn time via the snapshot cache. A new install
invalidates the relevant snapshot entry by mtime. A forced respawn always picks up current content.

**Trust boundary / project skills**: Phase 1 supports user-level skills only. Project-level
(`.agents/skills/` in repo) lands in Phase 2 behind an explicit trust-gate — prevents an
untrusted cloned repo from injecting skills automatically.

**Registry availability**: Install only requires GitHub to be reachable (or a local path). The
registry index is only used for search; offline installs via direct GitHub URL always work.

**Path traversal in bundles**: Hermes normalises all bundle-internal paths via `_normalize_bundle_path()`
before touching disk (`skills_hub.py:88–110`). Same validation must be applied: reject absolute
paths, `..` segments, Windows drive letters, and disallow nested paths for skill names.
