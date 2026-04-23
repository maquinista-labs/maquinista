# Soul Template Advanced Features

**Status:** Draft  
**Created:** 2026-04-18

## Overview

This document outlines advanced soul template capabilities beyond the MVP (create via dashboard, assign to agents). These are non-blocking improvements for future evaluation.

---

## 1. Template Editing

**Current:** Templates are immutable after creation.

**Desired:** Edit existing templates via dashboard.

### User Journey

1. Navigate to "Templates" section (new sidebar entry)
2. Click on a template card
3. Edit any field (name, tagline, role, goal, etc.)
4. Save → updates `soul_templates` + propagates to all `agent_souls` rows using this template

### Implementation Notes

- Add `PUT /api/soul-templates/[id]` endpoint
- When updating `template_id`, also update all `agent_souls` rows referencing it (or use a view/join that always fetches from `soul_templates` on render)
- Add "Templates" page in dashboard UI

---

## 2. Template Versioning

**Current:** Single version per template.

**Desired:** Track changes to templates over time.

### Use Cases

- Rollback to previous version
- Audit trail of who changed what
- A/B test different versions

### Implementation Notes

- Add `soul_template_versions` table:
  ```sql
  CREATE TABLE soul_template_versions (
    template_id TEXT REFERENCES soul_templates(id),
    version     INTEGER NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by  TEXT, -- operator user id
    name, tagline, role, goal, core_truths, boundaries, vibe, continuity, ...
    PRIMARY KEY (template_id, version)
  );
  ```
- On template edit, insert new row instead of updating
- Render always uses `version = (SELECT MAX(version) FROM soul_template_versions WHERE template_id = $1)`

---

## 3. Per-Agent Soul Customization

**Current:** Agent soul is a clone of template, but can diverge over time via `maquinista soul import`.

**Desired:** Dashboard UI to edit individual agent souls.

### User Journey

1. Open agent detail page
2. Click "Edit Soul" button
3. Modal shows current soul fields (editable)
4. Save → updates `agent_souls` row directly

### Implementation Notes

- Add `GET/PUT /api/agents/[id]/soul` endpoints
- Add "Soul" tab on agent detail page
- Fields: name, tagline, role, goal, core_truths, boundaries, vibe, continuity

---

## 4. Soul Template Categories/Tags

**Current:** Flat list of templates.

**Desired:** Group templates by category (e.g., "Engineering", "Review", "Support").

### Implementation Notes

- Add `category` column to `soul_templates`:
  ```sql
  ALTER TABLE soul_templates ADD COLUMN category TEXT;
  ```
- Update spawn modal dropdown to group by category
- Add filter on Templates page

---

## 5. Soul Template Import/Export

**Current:** Manual entry or migration-only seeding.

**Desired:** Import/export templates as JSON or Markdown.

### User Journey

1. Templates page → "Export" on a template → downloads JSON/MD
2. "Import" button → upload JSON/MD → creates new template

### Implementation Notes

- Add `POST /api/soul-templates/import` endpoint (accepts JSON)
- Add `GET /api/soul-templates/[id]/export` endpoint
- Markdown format matches `maquinista soul export` output for consistency

---

## 6. Default Template Selection

**Current:** One `is_default=TRUE` template (set via CLI).

**Desired:** Set default via dashboard.

### Implementation Notes

- Add `POST /api/soul-templates/[id]/set-default` endpoint
- Or reuse `PUT /api/soul-templates/[id]` with `is_default: true` body

---

## 7. Template Usage Analytics

**Desired:** See how many agents use each template.

### Implementation Notes

- Add computed field in Templates page:
  ```sql
  SELECT t.id, t.name, COUNT(a.id) as agent_count
  FROM soul_templates t
  LEFT JOIN agent_souls a ON a.template_id = t.id
  GROUP BY t.id;
  ```

---

## 8. Soul Rendering Preview

**Desired:** Preview what a soul will look like when rendered before creating an agent.

### User Journey

1. In spawn modal or template editor
2. Click "Preview" button
3. See rendered system prompt (same as `maquinista soul render` output)

### Implementation Notes

- Add `POST /api/soul-templates/preview` endpoint that accepts template fields and returns rendered output
- Call same `soul.ComposeForAgent` logic (may need Go-side endpoint, or replicate in TS)

---

## Priority Order

| Priority | Feature | Rationale |
|----------|---------|-----------|
| P0 (done) | Create template via UI | MVP shipped |
| P1 | Edit template | Common workflow |
| P1 | Per-agent soul edit | Existing gap |
| P2 | Import/Export | Operational flexibility |
| P2 | Categories | Organization |
| P3 | Versioning | Audit/recovery |
| P3 | Usage analytics | Observability |
| P4 | Preview | UX polish |

---

## Related Docs

- `plans/active/agent-soul-db-state.md` — DB schema and Phase 1/2 definition
- `cmd/maquinista/cmd_soul.go` — CLI soul commands
- `internal/soul/soul.go` — Go soul logic