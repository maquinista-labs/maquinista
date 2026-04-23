# Dashboard: Soul & Memory surface

> This plan adheres to §0 of `maquinista-v2.md`: **Postgres is the system of record**.

## Context

Soul and memory are fully live in the backend (`agent_souls`, `agent_blocks`,
`agent_memories`, `maquinista soul/memory` CLI) but invisible in the dashboard.
Users have no way to inspect or edit an agent's identity or memories without
dropping to the terminal.

The agent-detail page currently has two tabs: **Chat** and **Workspaces**. This
plan adds two more: **Soul** and **Memory**.

---

## Scope

Four commits, each independently mergeable and testable.

---

### Commit S.1 — Soul tab (read)

**New API route:** `GET /api/agents/[id]/soul`

```ts
SELECT name, tagline, role, goal, core_truths, boundaries, vibe, continuity,
       allow_delegation, max_iter, respect_context, version, updated_at
FROM agent_souls WHERE agent_id = $1
```

Returns `404` when no soul row exists for the agent (fresh spawn, soul not yet
created).

**New component:** `SoulCard`

Renders each soul field as a labelled read-only prose block (not a form yet —
S.2 adds editing). Fields and their labels:

| DB column | Display label |
|-----------|--------------|
| `name` | Name |
| `tagline` | Tagline |
| `role` | Role |
| `goal` | Goal |
| `core_truths` | Core truths |
| `boundaries` | Boundaries |
| `vibe` | Vibe |
| `continuity` | Continuity |

Boolean/numeric fields (`allow_delegation`, `max_iter`, `respect_context`) shown
as a compact metadata row beneath the prose blocks.

Empty string fields are rendered as a muted "—" so the layout doesn't collapse.
`version` + `updated_at` shown in the footer ("v3 · updated 2 hours ago").

**Wire into tabs:** `agent-detail-tabs.tsx` gains a third trigger/content pair
(`value="soul"`). `grid-cols-2` → `grid-cols-3`. URL param `?tab=soul` works.

**Files:**
- `src/app/api/agents/[id]/soul/route.ts` (new)
- `src/components/dash/soul-card.tsx` (new)
- `src/components/dash/agent-detail-tabs.tsx` (modified)

---

### Commit S.2 — Soul tab (edit)

Turns `SoulCard` into an inline-editable form. Each prose block gets an "Edit"
icon that switches it to a `<textarea>` with a char counter. Save sends a
`PATCH /api/agents/[id]/soul`.

**New API route:** `PATCH /api/agents/[id]/soul`

Accepts a partial body (any subset of the narrative fields). Runs `maquinista
soul edit` logic directly — upserts `agent_souls` with the new values, bumps
`version`, sets `updated_at = NOW()`.

Validation: `core_truths` / `boundaries` / `vibe` / `continuity` capped at
4000 chars each (matches the soul schema). Returns the updated row.

**Render preview button:** "Preview rendered soul" fetches
`GET /api/agents/[id]/soul/render` which calls `soul.ComposeForAgent` (the
same path `maquinista soul render` uses) and returns the composed system prompt
as plain text. Displayed in a scrollable `<pre>` sheet so operators can see
exactly what the agent sees at spawn.

**New API route:** `GET /api/agents/[id]/soul/render`

Calls `soul.ComposeForAgent(ctx, pool, agentID)`, returns `{text: string}`.

**Files:**
- `src/app/api/agents/[id]/soul/route.ts` (modified — add PATCH handler)
- `src/app/api/agents/[id]/soul/render/route.ts` (new)
- `src/components/dash/soul-card.tsx` (modified — add edit mode + preview)

---

### Commit M.1 — Memory tab (read)

**New API routes:**

`GET /api/agents/[id]/memory/blocks`
```sql
SELECT id, label, value, char_limit, read_only, description, version, updated_at
FROM agent_blocks WHERE agent_id = $1 ORDER BY label
```

`GET /api/agents/[id]/memory/passages?tier=&category=&q=&limit=50`
- When `q` is set: runs `memory.Search`; otherwise `memory.List`.
- Returns `{items: Memory[], total: int}`.

**New component:** `MemoryTab`

Two sections:

**Core blocks** — rendered as cards, one per block (`persona`, `human`,
`task-note`). Each card shows:
- Label + description
- Current value (prose, line-clamped to 4 lines with expand toggle)
- Char usage bar: `value.length / char_limit` as a thin progress bar
- `version` + `updated_at` in footer
- Read-only badge when `read_only = true`

**Archival passages** — a filterable list below the blocks:
- Tier filter pills: `long_term | daily | signal | (all)`
- Category filter pills: `feedback | project | reference | fact | preference | other | (all)`
- Search input (debounced 300ms, hits `?q=` param)
- Each row: pin icon, tier badge, category badge, title, body snippet (2 lines),
  `created_at` relative timestamp
- Pin toggle calls `POST /api/agents/[id]/memory/passages/[memId]/pin`
  (from `dashboard-rewind-actions.md` R.3, now absorbed here)

**Wire into tabs:** `agent-detail-tabs.tsx` gains a fourth trigger/content pair
(`value="memory"`). `grid-cols-3` → `grid-cols-4`. URL param `?tab=memory`.

**Files:**
- `src/app/api/agents/[id]/memory/blocks/route.ts` (new)
- `src/app/api/agents/[id]/memory/passages/route.ts` (new)
- `src/app/api/agents/[id]/memory/passages/[memId]/pin/route.ts` (new)
- `src/components/dash/memory-tab.tsx` (new)
- `src/components/dash/agent-detail-tabs.tsx` (modified)

---

### Commit M.2 — Memory tab (write)

Adds operator write actions to the memory tab.

**Core block editing:**
- Each block card gets an "Edit" button (hidden for `read_only` blocks).
- Opens an inline textarea with char-limit enforcement — turns red at 90%,
  blocks save at 100%.
- `PATCH /api/agents/[id]/memory/blocks/[label]` — calls
  `memory.ReplaceBlock` with `(oldContent="", newContent=newValue)` when the
  full value is replaced, or exposes a `PUT` that overwrites entirely (simpler
  for operator edits vs the agent's incremental append flow).

**Archival passage actions:**
- **Remember** button opens a sheet form: tier, category, title, body, pin
  toggle. `POST /api/agents/[id]/memory/passages` → `memory.Remember`.
- **Forget** — trash icon with confirm popover. `DELETE /api/agents/[id]/memory/passages/[memId]`
  → `memory.Forget`.

**Files:**
- `src/app/api/agents/[id]/memory/blocks/[label]/route.ts` (new — PATCH)
- `src/app/api/agents/[id]/memory/passages/route.ts` (modified — add POST)
- `src/app/api/agents/[id]/memory/passages/[memId]/route.ts` (new — DELETE)
- `src/components/dash/memory-tab.tsx` (modified — edit + remember + forget)

---

## Files summary

New API routes:
```
src/app/api/agents/[id]/soul/route.ts                       S.1 GET, S.2 PATCH
src/app/api/agents/[id]/soul/render/route.ts                S.2
src/app/api/agents/[id]/memory/blocks/route.ts              M.1
src/app/api/agents/[id]/memory/blocks/[label]/route.ts      M.2
src/app/api/agents/[id]/memory/passages/route.ts            M.1 GET, M.2 POST
src/app/api/agents/[id]/memory/passages/[memId]/route.ts    M.2 DELETE
src/app/api/agents/[id]/memory/passages/[memId]/pin/route.ts M.1
```

New components:
```
src/components/dash/soul-card.tsx                           S.1/S.2
src/components/dash/memory-tab.tsx                          M.1/M.2
```

Modified:
```
src/components/dash/agent-detail-tabs.tsx                   S.1, M.1
```

---

## Interaction with other plans

- Absorbs `dashboard-rewind-actions.md` R.3 (memory pin) — now lives in M.1.
- `soul.ComposeForAgent` (used in the render preview) is already shipped.
- All DB tables and Go CRUD (`memory.Remember`, `memory.List`, etc.) already
  shipped — this plan is purely a dashboard surface on top.

---

## Verification

- `maquinista agent add alice --persona "direct and concise"` → open alice in
  dashboard → Soul tab shows core_truths pre-filled from the persona. Edit vibe
  → save → `maquinista soul render alice` reflects the change.
- Memory tab shows three core blocks with char bars at 0% (freshly seeded).
  Send a message that triggers auto-flush → archival passage appears in the
  passages list without refresh (SSE invalidation).
- Pin a passage from the dashboard → `SELECT pinned FROM agent_memories WHERE
  id=...` returns `true`.
- Forget a passage → row disappears from the list.

## Open questions

1. **SSE invalidation scope.** The existing `useDashStream` hook handles
   conversation events. Memory and soul changes don't currently emit SSE
   events. Add `agent_blocks_updated` / `agent_memories_updated` NOTIFY
   triggers, or poll on a short interval (5 s) for the memory tab only?
2. **Render preview latency.** `ComposeForAgent` is fast but hits the DB.
   Cache the render output for a few seconds on the API side to avoid hammering
   on rapid re-opens of the preview sheet.
3. **Block edit UX for `read_only` blocks.** Show a tooltip explaining why
   the edit button is absent, or hide it entirely?
