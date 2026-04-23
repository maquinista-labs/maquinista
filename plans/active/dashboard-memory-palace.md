# Dashboard: Memory Palace (wiki / Obsidian-like view)

**Status:** Draft — no blockers, but depends on a reasonable volume of
`agent_memories` rows existing first. Recommend implementing after
`dashboard-soul-memory.md` ships (M.1/M.2 give operators a way to create
and curate passages; the Palace view gives them a structured way to navigate
them).

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**.

---

## Context

openclaw's Memory Palace is a compiled knowledge vault organized into five
page kinds (entity / concept / source / synthesis / report) with claims,
evidence, contradictions, and open questions per entry. It's the closest
thing in the agent-tool ecosystem to an Obsidian-style linked knowledge graph
surfaced inside a dashboard.

Maquinista's equivalent is `agent_memories` with its `dimension`, `tier`,
`category`, `tags`, and FTS index. The raw data is richer in some ways
(structured category taxonomy, per-passage scoring, archive membership) but
the dashboard currently offers nothing beyond a flat filterable list (M.1
in `dashboard-soul-memory.md`).

The Memory Palace is a second, complementary view of the same data:
**clustered by semantic kind, navigable by tag/category graph, with
cross-links between related passages**. Think Obsidian's graph view but
anchored to the agent's archival memory rather than a markdown vault.

---

## Scope

Three progressive commits. Each is usable standalone; later ones add depth.

---

### Commit P.1 — Clustered gallery view

Replace (or add alongside) the flat passages list in M.1 with a **gallery
grouped by category**. No new data model — just a different rendering of
`agent_memories`.

**Layout:**

```
┌─ feedback (3) ──────────────────────────┐  ┌─ project (7) ───────────────┐
│ • Don't summarise the diff at end…  📌  │  │ • Merge freeze starts Apr 5  │
│ • Terse replies preferred in Telegram   │  │ • Pipeline bugs in Linear …  │
│ • Agent should prefer snake_case        │  │  …                           │
└─────────────────────────────────────────┘  └─────────────────────────────┘
┌─ reference (2) ─────────────────────────┐  ┌─ fact (12) ─────────────────┐
│ • Grafana board at grafana.internal/…   │  │ • Claude 4.6 has 1M context  │
│ • Pipeline bugs in Linear "INGEST"      │  │  …                           │
└─────────────────────────────────────────┘  └─────────────────────────────┘
```

Each cluster:
- Header shows category label + count + "add" shortcut
- Cards show title, 2-line body snippet, tier badge, pin icon, relative
  timestamp
- Pinned rows float to top within each cluster
- Click opens a slide-over with the full passage (same as M.1 row detail)

**Toggle:** Gallery (grid) ↔ List (M.1 table) switch in the tab header.
Default is list; gallery is opt-in.

**API:** reuses `GET /api/agents/[id]/memory/passages` — adds `?group=category`
param that returns `{category: string, items: Memory[]}[]` instead of a flat
array.

**Files:**
- `src/app/api/agents/[id]/memory/passages/route.ts` (modified — add groupBy)
- `src/components/dash/memory-gallery.tsx` (new)
- `src/components/dash/memory-tab.tsx` (modified — toggle + gallery slot)

---

### Commit P.2 — Tag graph + cross-links

Add a **tag-based graph panel** to the memory tab. Nodes are tags (from
`agent_memories.tags`); edges connect any two tags that co-occur on the same
passage. Clicking a node filters the gallery/list to passages sharing that tag.

**Graph rendering:** use `@xyflow/react` (already a common dependency in
Next.js dashboards) or a simpler force-layout with `d3-force`. Nodes sized
by passage count; edges weighted by co-occurrence frequency.

**Cross-links within passages:** when a passage body mentions a word that
matches another passage's title (case-insensitive), render it as a blue
inline link in the slide-over detail view. Clicking navigates to the
referenced passage (no new DB query — just a client-side scan of the
already-loaded passages).

**New API route:** `GET /api/agents/[id]/memory/tag-graph`

```ts
SELECT tags, COUNT(*) as count
FROM agent_memories
WHERE agent_id = $1
  AND (expires_at IS NULL OR expires_at > NOW())
  AND array_length(tags, 1) > 0
GROUP BY tags
```

Client builds the co-occurrence graph from the returned tag arrays.

**Files:**
- `src/app/api/agents/[id]/memory/tag-graph/route.ts` (new)
- `src/components/dash/memory-graph.tsx` (new — force graph)
- `src/components/dash/memory-passage-detail.tsx` (new — slide-over with
  cross-link rendering, split out from memory-tab.tsx)
- `src/components/dash/memory-tab.tsx` (modified — graph panel slot)

---

### Commit P.3 — Synthesis view (openclaw Memory Palace equivalent)

The richest commit. Adds a **Synthesis** sub-tab inside the Memory tab that
renders a compiled knowledge view inspired by openclaw's Memory Palace: entries
grouped by kind (person / project / concept / lesson / tool), each with a
claims list and open questions derived from the passage bodies.

**Data model addition:** no new tables. Introduce a convention:
- A passage with `category='reference'` and `tags=['entity:…']` is a
  "person or project" entry.
- A passage with `category='feedback'` is a "lesson" entry.
- A passage with `category='fact'` + `tags=['concept:…']` is a "concept".

Alternatively (cleaner): add a `kind` column to `agent_memories`:

```sql
-- migration 033_memory_kind.sql
ALTER TABLE agent_memories
  ADD COLUMN IF NOT EXISTS kind TEXT
    CHECK (kind IN ('entity','concept','lesson','tool','source','synthesis'));
CREATE INDEX IF NOT EXISTS idx_agent_memories_kind ON agent_memories(agent_id, kind)
  WHERE kind IS NOT NULL;
```

`kind` is optional — existing rows default to NULL and render in the flat
list/gallery. Passages with a `kind` also appear in the Synthesis view.

**Synthesis view layout:**

```
Entities (4)          Concepts (6)         Lessons (11)
─────────────         ──────────────       ────────────
Otavio                prefix caching       Don't summarise diffs
  role: operator      • reduces latency    Terse in Telegram
  timezone: pt-BR     • Claude 4.6+        Use snake_case
  prefers terse       Open Qs: best model? …

Pico (cat)            tmux sessions
  ...                 • per-agent windows
                      • WaitForReady pat.
```

Each kind block is a masonry column. Each entry card shows:
- Title (bold)
- Bullet-list of body sentences (auto-parsed: split on `. `)
- Open questions (lines ending in `?`)
- Tags as chips
- "Edit in passages" link → opens the flat list filtered to this entry

**Passage editor enhancement (M.2 follow-on):** add a `kind` selector
dropdown to the Remember form and the slide-over edit mode.

**New API route:** `GET /api/agents/[id]/memory/synthesis`
Returns passages where `kind IS NOT NULL`, grouped by kind.

**Files:**
- `internal/db/migrations/033_memory_kind.sql` (new)
- `src/app/api/agents/[id]/memory/synthesis/route.ts` (new)
- `src/components/dash/memory-synthesis.tsx` (new — masonry kind view)
- `src/components/dash/memory-tab.tsx` (modified — Synthesis sub-tab)
- `src/components/dash/memory-passage-detail.tsx` (modified — kind selector)

---

## Comparison with openclaw Memory Palace

| Feature | openclaw | maquinista P.1–P.3 |
|---------|----------|---------------------|
| Grouped by kind | ✅ entity/concept/source/synthesis/report | ✅ P.3 (entity/concept/lesson/tool/source/synthesis) |
| Claims per entry | ✅ structured `WikiClaim[]` with evidence + confidence | ⚠ body parsed as bullet sentences (no structured claim model) |
| Cross-links between entries | ✅ `linkTargets[]` compiled at wiki-build time | ✅ P.2 (client-side title match, lighter) |
| Tag/concept graph | ❌ no graph view in openclaw | ✅ P.2 (co-occurrence force graph) |
| Category gallery | ❌ kind-only, no category dimension | ✅ P.1 (category-grouped cards) |
| Evidence / provenance | ✅ per-claim evidence with source + line refs | ❌ only `source` + `source_ref` text fields |
| Obsidian-style backlinks | ❌ no | ✅ P.2 (inline cross-links in detail view) |
| Edit in UI | ✅ full file editor | ✅ M.2 (structured form) |
| Dreaming feed in view | ✅ advanced sub-tab with score breakdowns | ❌ postponed |

**Main gap vs openclaw:** the structured claim model. openclaw's `WikiClaim`
has `text`, `status`, `confidence`, and an `evidence[]` array with source
paths and line numbers. Maquinista passages have a free-text body. P.3 parses
body into pseudo-claims by splitting on sentences, which works for short
focused passages but breaks down for paragraph-style bodies. A future `claims`
JSONB column on `agent_memories` would fix this properly.

---

## Files summary

```
internal/db/migrations/033_memory_kind.sql                      P.3
src/app/api/agents/[id]/memory/passages/route.ts                P.1 (modified)
src/app/api/agents/[id]/memory/tag-graph/route.ts               P.2 (new)
src/app/api/agents/[id]/memory/synthesis/route.ts               P.3 (new)
src/components/dash/memory-gallery.tsx                          P.1 (new)
src/components/dash/memory-graph.tsx                            P.2 (new)
src/components/dash/memory-passage-detail.tsx                   P.2 (new)
src/components/dash/memory-synthesis.tsx                        P.3 (new)
src/components/dash/memory-tab.tsx                              P.1–P.3 (modified)
```

---

## Open questions

1. **Claims model.** Body-splitting into pseudo-claims is fragile. Add a
   `claims JSONB` column to `agent_memories` so the agent (and operator) can
   write structured `[{text, confidence, status}]` arrays? Adds migration and
   tool-surface changes but makes P.3 much cleaner.
2. **Graph rendering library.** `@xyflow/react` is polished but heavy (~300 KB).
   A hand-rolled SVG force layout is smaller. Decide based on whether the
   dashboard already has a graph dependency anywhere.
3. **Kind vs category.** Should `kind` replace `category`, or coexist? openclaw
   uses kind as the primary organizer; maquinista's category taxonomy
   (`feedback/project/reference/fact/preference/other`) already covers similar
   ground. May be redundant — P.3 could reuse `category` as the kind axis
   and drop the new column entirely.
4. **Archive-scoped synthesis.** P.3 shows per-agent synthesis. Should the
   Synthesis view optionally include archive-accessible passages (from
   `agent_archives` / `archive_members`)? Natural extension once archives
   are populated.
