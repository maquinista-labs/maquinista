# Pipeline role souls (role-souls)

Sources:

- `ADRs/0005-linear-pr-iteration-pipeline.md` §Implementation tasks:169-170 — EX-02 "four
  templates + prompt contract (validators, `maquinista-done`, verdict vocabulary), worker
  spec-first enforcement"
- `ADRs/0005` §Architecture:140-144 — the five role names: pipeline-worker (tlc-spec-lean
  contract, default runner pi), pipeline-reviewer + pipeline-arbiter (high-reasoning;
  reviewer fresh per round, never the author), pipeline-fixer, pipeline-merger
- `ADRs/0005` reqs 5/7/10 (:75-91) — reviewer materials + verdict vocabulary
  (approve / request_changes / needs_human), round cap → Needs Human, merge proposal +
  `maquinista approve` at `PIPELINE_AUTO_MERGE=0`; :86-88 + :149-153 merger rebase/conflict
- `internal/db/migrations/028_seed_default_agents.sql` — the seed style (catalog entries
  only, `ON CONFLICT (id) DO NOTHING`, no agent rows)
- `internal/db/migrations/016_agent_souls.sql:12-26` — soul_templates DDL (incl. extras
  JSONB)
- `internal/soul/soul.go:155-183` — CloneFromTemplate copies goal/truths/boundaries/extras
  verbatim, clone independent afterwards; `internal/soul/compose.go:38+` — extras render
  into the spawned system prompt
- `internal/prompt/prompt.go:45-50` — maquinista-observe / -handoff / -done protocol
- `internal/soul/soul_test.go:12-28` — dbtest.PgContainer + RunMigrations test pattern
- `arch/pipeline.md` — docs landing (Intake / Mirror / Env contract / TODO sections exist)
- `.specs/features/ticket-abstraction/` — format precedent

Review mode: tracked by Linear MAQ-3 (EX-02, In Progress). Ships as its own PR on branch
`role-souls`.

## Problem

EX-01 shipped the machinery (claim loop, board mirror, `tasks.review_rounds`), but the
roles that will consume it do not exist: EX-03 needs a reviewer soul to dispatch fresh per
round, EX-04 needs the fixer, EX-05 needs the merger, and the worker soul must enforce
spec-first tlc discipline from the first pilot task (EX-07). Nothing downstream can be
built until the role templates exist and the two cross-exercise contracts are frozen:
the machine-parseable verdict line EX-03 will parse, and the extras dispatch keys EX-03
will read.

When this ships: five pipeline soul templates are seeded catalog entries (028 style — no
agent rows), each carrying its role's prompt contract, and `arch/pipeline.md` documents
the set.

## Out of scope

- Any Go dispatch code — verdict PARSING, zero-author checks, dispatch wiring (EX-03);
  fixer re-claim loop (EX-04); merge-mode code (EX-05); Telegram plumbing (EX-06)
- Inserting agent rows or spawning pipeline agents — catalog entries only, per the 028
  pattern ("actual agent rows are NOT inserted here")
- Editing existing templates (default, coordinator, planner, coder)
- Runner/model selection logic — souls stay runner-agnostic (ADR-0005 revisit triggers);
  EX-03 reads `reasoning_class` and picks
- Verifying the prompts produce good agent behavior — that is EX-07's pilot; this feature
  freezes and seeds the contracts

## Assumptions

- ADR-0005 §Implementation tasks says EX-02 ships "four templates" while §Architecture
  names five roles — the count groups reviewer+arbiter in one bullet. Chosen default:
  seed all five (the Decision section is normative; merger is a catalog row, not dispatch
  code, so seeding it now costs one INSERT and keeps a single migration)
- Default runner is pi for every pipeline role (box default; ADR names pi for the worker).
  Reviewer/arbiter distinguish themselves via `reasoning_class: high`, which EX-03
  resolves to a model at dispatch — no model names in the souls
- Verdict sessions reach the parser as plain assistant text via the monitor → agent_outbox
  path, so a final-line contract survives relay and markdown rendering
- `PIPELINE_AUTO_MERGE=0` is the v1 default (ADR-0005 req 10), so the merger prompt states
  proposal-then-wait as the primary behavior

**Open questions:** none — the five-vs-four template count is the only candidate and is
resolved by Assumption 1 (superset; Decision section wins).

## Criteria

Grouped by slice - one observable outcome each, never a layer. Numbering runs
across the whole plan.

### S1: seed migration (P1)

**Acceptance Criteria**

1. WHEN RunMigrations applies on a clean database THEN soul_templates SHALL gain exactly
   five rows with ids pipeline-worker, pipeline-reviewer, pipeline-arbiter, pipeline-fixer
   and pipeline-merger, every one with is_default FALSE
2. IF 035_seed_pipeline_souls.sql runs a second time on the same database THEN its re-apply SHALL be a no-op:
   the soul_templates row count unchanged, with the pre-existing rows (default,
   coordinator, planner, coder) identical before and after
3. WHEN soul.CreateFromTemplate clones pipeline-worker THEN the clone SHALL carry the
   template's role, goal, core_truths, boundaries and extras verbatim into a new
   agent_souls row
15. WHEN Load reads an agent_souls row carrying non-empty jsonb extras THEN the returned soul SHALL expose
    those extras decoded (amendment 02/10, found during build: Load
    scanned jsonb into any and asserted []byte — pgx v5 returns map[string]any, so the
    assertion silently emptied extras on every clone read-back; the row was correct, the
    read seam was not)

**Independent test:** `go test ./internal/soul/ -run TestMigration035 -v` plus
`go test ./internal/soul/ -run TestPipelineTemplateClone -v`.

### S2: worker contract (P1)

**Acceptance Criteria**

4. WHEN the pipeline-worker template is rendered THEN its prompt SHALL contain the
   spec-first rule that a plan.md plus checks.md pair under .specs/features/<slug>/ SHALL
   exist and pass validate_plan.py and validate_checks.py before any implementation code
   is written
5. WHEN the pipeline-worker template is rendered THEN its prompt SHALL name the task tools
   maquinista-done, maquinista-observe and maquinista-handoff, and SHALL bind completion
   to every named proof in checks.md green at HEAD and check_commit.py passing per commit
6. WHEN the pipeline-worker template is rendered THEN its boundaries SHALL forbid scope
   beyond the assigned task and forbid editing checks.md claims to turn a red proof green

**Independent test:** `go test ./internal/soul/ -run TestPipelineWorkerContract -v`.

### S3: reviewer + arbiter contract (P1)

**Acceptance Criteria**

7. WHEN the pipeline-reviewer template is rendered THEN its prompt SHALL require reviewing
   the task description, plan.md, checks.md, verification.md and the full diff, and SHALL
   forbid basing a verdict on the builder's summary alone
8. WHEN a reviewer or arbiter session completes THEN its prompt SHALL have required the
   output to end with exactly one verdict line: the literal token VERDICT followed by
   colon and space and one of approve, request_changes, needs_human — the contract EX-03
   parses
9. WHEN the pipeline-arbiter template is rendered THEN its goal SHALL define high-reasoning
   adjudication of a contested or repeated request_changes, returning the same three-value
   verdict vocabulary with file:line evidence per finding

**Independent test:** `go test ./internal/soul/ -run TestPipelineVerdictContract -v`.

### S4: fixer + merger contract (P2)

**Acceptance Criteria**

10. WHEN the pipeline-fixer template is rendered THEN its prompt SHALL scope work to
    resolving the reviewer's numbered findings in the same worktree and PR, and its
    boundaries SHALL forbid spec-obligation edits and scope-expanding refactors
11. WHEN the pipeline-merger template is rendered THEN its prompt SHALL require rebasing
    onto origin/main, gating the squash merge on gh pr checks green, and on rebase
    conflict SHALL require either resolving or parking with the conflict file list
12. WHEN the pipeline-merger template is rendered THEN its prompt SHALL state the v1
    default as primary behavior: post a merge proposal and wait for the operator's
    maquinista approve before merging

**Independent test:** `go test ./internal/soul/ -run TestPipelineFixerMergerContract -v`.

### S5: dispatch hints + docs (P2)

**Acceptance Criteria**

13. WHEN the pipeline templates are seeded THEN each SHALL carry extras keys
    default_runner and reasoning_class with value pairs pi standard (worker, fixer,
    merger) and pi high (reviewer, arbiter) — the literal key names are the EX-03
    dispatch contract
14. WHEN the docs land THEN arch/pipeline.md SHALL contain a Role souls section listing
    the five templates, the verdict line contract and the extras keys

**Independent test:** `go test ./internal/soul/ -run TestPipelineDispatchExtras -v` plus
`rg -c 'Role souls' arch/pipeline.md`.

## Traceability

| ID | Slice | Criteria | Status |
| --- | --- | --- | --- |
| RS-01 | S1 | 1–3, 15 | Pending |
| RS-02 | S2 | 4–6 | Pending |
| RS-03 | S3 | 7–9 | Pending |
| RS-04 | S4 | 10–12 | Pending |
| RS-05 | S5 | 13–14 | Pending |

## Observable

Every item of every surface this feature exposes. `n/a` needs its reason.

| Surface | Decision | Landing |
| --- | --- | --- |
| soul_templates catalog | +5 pipeline role templates, all is_default FALSE | AC 1 |
| Dashboard spawn picker | pipeline templates selectable as archetypes like 028's | AC 1 |
| Spawned agent prompt | contract text + extras key/value pairs render into system prompt | AC 4–13 |
| Clone read-back seam | extras survive Load (jsonb scan fix) | AC 15 |
| Verdict interface | final-line VERDICT contract, consumed by EX-03 | AC 8 |
| Dispatch interface | extras keys default_runner + reasoning_class, consumed by EX-03 | AC 13 |
| Bot commands / payloads | existing flows untouched | n/a - no bot surface added |
| CLI | `maquinista soul render` unchanged | n/a - no new CLI surface |
| Schema | none — 016 DDL untouched, seed data only | n/a - catalog rows only |

## Flow

1. `035_seed_pipeline_souls.sql` (new, no door) → `soul_templates` (exists)
2. orchestrator engine / EnsureAgent (exists) → `soul.CreateFromTemplate` (exists) →
   `agent_souls` clone (exists) — EX-03 will dispatch against these template ids
3. `maquinista soul render <agent-id>` (exists) → runner system prompt carries the contract
4. reviewer/arbiter session outbox (exists) → "VERDICT: <value>" final line → EX-03 parser
   (future, out of scope)

## Relations

- soul_templates 1:N agent_souls via template_id; the clone is independent after creation
  — no propagation (existing primitive, soul.go:155-183)
- pipeline templates are catalog entries exactly like 028's: the migration inserts no
  agent rows; agent creation stays in Go at dispatch time
- EX-03/EX-04/EX-05 are downstream consumers: the verdict line and extras keys are defined
  here, implemented there; this plan freezes, never implements

## Surface

None external — one migration, one docs section. The catalog rows appear in the dashboard
"New Agent" picker (recorded under Observable); no route, API or payload changes.

## Landing

| One-way door | Literal shape | Alternative rejected |
| --- | --- | --- |
| Verdict line contract | prompt requires the session's final line to be `VERDICT: approve` or `VERDICT: request_changes` or `VERDICT: needs_human` — EX-03's parser and the round-accounting transitions key on this literal | free-form verdicts or JSON verdict blocks — fragile across runner formatting, markdown and relay truncation; the plain final line survives both |
| Dispatch hint keys | extras JSONB keys `default_runner` + `reasoning_class` on every pipeline template | per-role runner/model constants in Go dispatch code — souls must stay runner-agnostic per ADR-0005's revisit triggers; EX-03 re-binds without touching souls |
| Five templates, not four | ids pipeline-worker, pipeline-reviewer, pipeline-arbiter, pipeline-fixer, pipeline-merger in one 035 migration | seeding four and deferring merger to EX-05 — the Decision section names five roles; a deferred insert would split the catalog across migrations for no gain |
| Contract lives in soul text, not code | validators/maquinista-done/verdict rules as goal/core_truths/boundaries prose, rendered per spawn | a Go-side prompt injector for pipeline agents — the soul render path already exists and is what EX-03's spawned agents will receive |

Nothing else is hard to reverse: template rows are catalog data, editable or deletable
without schema change.

## Impact

| Front | What changes |
| --- | --- |
| domain | new terms: role soul (pipeline-*), verdict line (VERDICT:), dispatch hint (extras keys) |
| stored data | +5 soul_templates rows; no agent rows; no schema change |
| existing behavior | none until EX-03 dispatches against these ids; dashboard picker lists five more archetypes |
| ops | none — no env, no deploy surface; migration ships with the next deploy as a no-op until cloned |
| docs | arch/pipeline.md gains a Role souls section; ADR-0005 untouched |
