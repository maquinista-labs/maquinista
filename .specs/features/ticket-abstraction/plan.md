# Ticket-provider abstraction (ticket-abstraction)

Sources:

- PR #1 review directive (Otavio, 01/10/2026): "Code has references to linear it should be
  abstract to a ticket/kanban system that we could replace with integrations other than
  linear if needed" — **must-fix before merge**: core pipeline code must be provider-agnostic
- `ADRs/0005-linear-pr-iteration-pipeline.md` — the pipeline decision this refines; shape
  stays, vendor coupling moves behind a seam (formalized in ADR-0006)
- `internal/pipeline/linear.go` — current Linear GraphQL client (becomes the Linear provider)
- `internal/pipeline/bridge.go` / `sync.go` — intake + mirror, to be neutralized
- `internal/db/migrations/034_linear_bridge.sql` — schema to neutralize; unmerged PR, so the
  in-place rename is legal ("never edit existing" means shipped)
- `.specs/features/linear-bridge/` — format precedent

Review mode: continues inside PR #1 (MAQ-2 already In Review). No new Linear card — this is
a pre-merge revision of the same EX-01 slice, tracked by this spec.

## Problem

EX-01 as implemented couples maquinista to Linear in four places that outlive any single
vendor: the schema (`linear_issue_map`, `linear_issue_id`), the config surface
(`MAQUINISTA_LINEAR_*`, `LINEAR_API_KEY`), the core pipeline files (bridge/sync construct a
`LinearClient` and speak Linear column names), and the task metadata keys
(`linear_issue_id`/`linear_url`). Swapping in Jira/Height/Shortcut later would mean a schema
migration plus edits in every layer, and EX-02+ are about to build on the bridge — the seam
has to exist before they do.

When this ships: `internal/pipeline` core (bridge, sync, config, cmd wiring) contains no
'linear' occurrence outside the provider file and its test; adding a second tracker is +1
provider file and +1 factory case.

## Out of scope

- Any second provider implementation — the seam is proven by negative checks + interface
  tests, not by a Jira port
- Renaming shipped migrations — 034 is unmerged; in-place rename is correct here
- `internal/webhooks/ratelimit.go` ("refills linearly" — false positive) and
  `.specs/features/pi-integration/plan.md` (history)
- Webhook-driven intake — polling stays; ADR-0005's 60 s bound unchanged

## Assumptions

- Linear column names for the MAQ board remain "In Progress", "In Review", "Changes
  Requested", "Needs Human", "Done" — the canonical set equals Linear's names for this team,
  so storing canonical names in `pending_state`/`last_synced_state` stays human-readable and
  diff-compatible with already-written rows
- Provider construction is synchronous and cheap (HTTP client + key); no provider needs
  background resources in v1
- The legacy `LINEAR_API_KEY` fallback may live inside the Linear provider (in-zone) as
  migration sugar; core never reads it

**Open questions:** none — the legacy key fallback was the only candidate and is resolved
as AC 7.

## Criteria

Grouped by slice - one observable outcome each, never a layer. Numbering runs
across the whole plan.

### S1: neutral schema (P1)

**Acceptance Criteria**

1. WHEN RunMigrations applies on a clean database THEN the applied set SHALL contain
   `034_ticket_bridge.sql` (and not `034_linear_bridge.sql`), creating table
   ticket_issue_map with issue_id TEXT PRIMARY KEY, issue_key TEXT NOT NULL, team_id TEXT
   NOT NULL, task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
   last_synced_state TEXT, pending_state TEXT, attempts INTEGER NOT NULL DEFAULT 0,
   next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), synced_at TIMESTAMPTZ, created_at
   and updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
2. WHEN RunMigrations applies THEN tasks SHALL gain review_rounds INTEGER NOT NULL DEFAULT 0
   (unchanged from the previous revision)
3. IF a tasks row is deleted THEN its ticket_issue_map row SHALL be removed by cascade
4. WHEN the repo tree is searched for 'linear' under internal/db/ THEN the count SHALL be 0

**Independent test:** `go test ./internal/db/ -run TestMigration034 -v` plus
`rg -i linear internal/db/ | wc -l` (rc pipe-free via `; echo rc=$?`).

### S2: provider seam (P1)

**Acceptance Criteria**

5. WHEN internal/pipeline/provider.go compiles THEN it SHALL define TicketProvider with
   IntakeIssues(ctx, teamID) ([]Issue, error), Columns(ctx, teamID) (map[Column]string,
   error) returning canonical→provider-column-ID, and SetIssueColumn(ctx, issueID,
   columnID) error; Issue{ID, Key, Title, Description, URL}; Column enum InProgress,
   InReview, ChangesRequested, NeedsHuman, Done whose String() SHALL return "In Progress",
   "In Review", "Changes Requested", "Needs Human", "Done"
6. A case-insensitive search for 'linear' under internal/pipeline/ SHALL match exactly
   internal/pipeline/linear.go and internal/pipeline/linear_test.go
7. WHEN the Linear provider resolves credentials THEN it SHALL read
   MAQUINISTA_TICKETS_API_KEY first and fall back to LINEAR_API_KEY; all transport behavior
   (raw Authorization header, data-envelope unwrap, GQL/HTTP error surfacing, Todo+pipeline
   intake filter) SHALL be unchanged
8. WHEN NewProvider is called with "linear" THEN it SHALL return a TicketProvider; with an
   unknown name THEN it SHALL error naming the supported providers
9. WHEN bridge.go claims an unmapped issue THEN it SHALL insert tasks metadata keys
   ticket_issue_id and ticket_url, title prefixed "[<Key>] ", and a ticket_issue_map row
   with pending_state "In Progress"; re-claim SHALL stay a no-op; the scheduler ready-claim
   query SHALL still match
10. WHEN sync reconciles a due row THEN it SHALL store canonical column names only in
    pending_state/last_synced_state, preserving the explicit-pending precedence, diff-based
    due-ness, 15 s doubling / 10 min cap backoff and unmapped-skip semantics

**Independent test:** `go test ./internal/pipeline/ -v` (TestColumn, TestNewProvider,
TestLinearProvider, TestClaim, TestSync, TestTicketsConfig) plus
`rg -il linear internal/pipeline/ | sort`.

### S3: wiring + docs (P2)

**Acceptance Criteria**

11. WHEN the operator configures the bridge THEN the env contract SHALL be
    MAQUINISTA_TICKETS_PROVIDER (default "linear"), MAQUINISTA_TICKETS_API_KEY,
    MAQUINISTA_TICKETS_TEAM_ID, MAQUINISTA_TICKETS_PROJECT (fallback MAQUINISTA_PROJECT),
    MAQUINISTA_TICKETS_POLL (60 s default); the gate SHALL require API key + team;
    MAQUINISTA_LINEAR_* SHALL not appear anywhere under internal/ or cmd/
12. WHEN cmd_start enables the bridge THEN it SHALL construct the provider via
    pipeline.NewProvider and contain no 'linear' occurrence
13. WHEN the docs land THEN arch/pipeline.md SHALL be provider-neutral with the
    MAQUINISTA_TICKETS_* table, ADRs/0006-ticket-provider-abstraction.md SHALL exist
    (context/decision/consequences, superseding ADR-0005's Linear-specific design bits),
    and arch/README.md + ADRs/README.md SHALL index the new ADR

**Independent test:** `go test ./internal/pipeline/ -run TestTicketsConfig -v`,
`rg -i linear cmd/ | wc -l`, `rg -i 'MAQUINISTA_LINEAR_' internal/ cmd/ | wc -l`,
`test -f ADRs/0006-ticket-provider-abstraction.md`.

## Traceability

| ID | Slice | Criteria | Status |
| --- | --- | --- | --- |
| TA-01 | S1 | 1–4 | Pending |
| TA-02 | S2 | 5–10 | Pending |
| TA-03 | S3 | 11–13 | Pending |

## Observable

Every item of every surface this feature exposes. `n/a` needs its reason.

| Surface | Decision | Landing |
| --- | --- | --- |
| Operator env contract | MAQUINISTA_TICKETS_* replaces MAQUINISTA_LINEAR_*; legacy key fallback inside the Linear provider | AC 11, AC 7 |
| Board behavior | unchanged — same columns, same mapping, same backoff | AC 9, AC 10 |
| Schema | ticket_issue_map replaces linear_issue_map (unmerged migration, rename in place) | AC 1 |
| Vendor swap | new tracker = provider file + factory case; core untouched | AC 5, AC 6, AC 8 |
| Bot commands / payloads | existing flows untouched | n/a - no bot surface added |
| Dashboard | existing - reads tasks/agents generically | n/a - no new dashboard UI |
| CLI | existing - no maquinista subcommand changes | n/a - no new CLI surface |

## Flow

1. `MAQUINISTA_TICKETS_PROVIDER` → `pipeline.NewProvider` (new) → TicketProvider ("linear"
   resolves to the Linear provider in `internal/pipeline/linear.go`, rewritten)
2. -> RunBridge 60 s tick: IntakeIssues -> ClaimIssue inserts tasks (ready) +
   ticket_issue_map (`034_ticket_bridge.sql`)
3. -> task-scheduler (exists) -> EnsureAgent -> agent_inbox — unchanged downstream
4. -> RunSync 10 s tick: JOIN ticket_issue_map↔tasks -> DerivedState → canonical Column ->
   Columns() lookup -> SetIssueColumn -> board; canonical names stored on both sides

## Relations

- ticket_issue_map is 1:1 with tasks (UNIQUE task_id, FK ON DELETE CASCADE); the PK is the
  provider issue ID, making the INSERT the single-owner claim (unchanged primitive)
- DerivedState (tasks.status → canonical Column) stays in core; canonical→vendor column
  names move into the provider
- ADR-0006 supersedes the Linear-specific parts of ADR-0005 §Design; ADR-0005's obligations
  (60 s intake bound, task-row-first claim, board mirror with retry) carry over unchanged

## Surface

None external — two internal goroutines, one table, one factory. The env contract is the
operator surface, recorded under Observable; no route, API or payload changes.

## Landing

| One-way door | Literal shape | Alternative rejected |
| --- | --- | --- |
| Neutral table name | `CREATE TABLE ticket_issue_map (issue_id TEXT PRIMARY KEY, issue_key TEXT NOT NULL, ...)` in `034_ticket_bridge.sql` | Keeping linear_issue_map — the vendor name would survive every future schema doc and EX-02+ queries |
| Canonical columns in core | `Column` enum + `String()` in `provider.go`; sync stores canonical names only | Storing vendor column names — a provider swap would strand every stored state value |
| Neutral env namespace | `MAQUINISTA_TICKETS_*` (+ provider-internal legacy key fallback) | Keeping `MAQUINISTA_LINEAR_*` with a shim — two names for one thing, forever |
| Provider interface, not registry of plugins | static `NewProvider` switch in core, implementations in-package | Dynamic plugin loading — one caller, three expected providers; a switch is auditable |
| Metadata keys renamed pre-merge | `ticket_issue_id` / `ticket_url` | Alias reads (linear_issue_id OR ticket_issue_id) — no rows exist yet in any deployed DB; aliases would be permanent debt |

Nothing else is hard to reverse: the seam is one interface, one factory case per provider.

## Impact

| Front | What changes |
| --- | --- |
| domain | new terms: ticket provider (TicketProvider), canonical column (Column), ticket_issue_map |
| stored data | migration 034 renamed 034_ticket_bridge.sql; linear_issue_map → ticket_issue_map (issue_id, issue_key); no deployed rows exist |
| existing behavior | board semantics unchanged; config surface renamed; LINEAR_API_KEY still honored by the Linear provider |
| ops | barceloneta env must switch to MAQUINISTA_TICKETS_TEAM_ID (+ optional MAQUINISTA_TICKETS_API_KEY); LINEAR_API_KEY alone keeps working via fallback |
| docs | arch/pipeline.md neutralized; ADR-0006 added; ADR-0005 untouched (history) |
