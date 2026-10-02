# ADR-0006: Ticket-Provider Abstraction — vendor-neutral pipeline core behind a TicketProvider seam

- **Status:** Proposto (pending Otavio's ok)
- **Date:** 2026-10-01
- **Deciders:** Otavio
- **Scope:** `internal/pipeline/` core (bridge, sync, config, cmd wiring), migration 034, the operator env contract, task metadata keys
- **Supersedes:** the Linear-specific parts of ADR-0005 §Design (linear-bridge naming, `MAQUINISTA_LINEAR_*` env, Linear column names as stored values). ADR-0005's obligations carry over unchanged: 60 s intake bound, task-row-first single-owner claim, board mirror with retry, agents never call the ticket system directly.
- **Depends on:** ADR-0005 (pipeline decision), migration 034 (unmerged PR #1 — the window that makes the in-place rename legal)

## Context

ADR-0005 chose Linear as the human-facing board and EX-01 implemented the
bridge against it. PR #1 review (Otavio, 01/10/2026) flagged the coupling:
"Code has references to linear it should be abstract to a ticket/kanban system
that we could replace with integrations other than linear if needed."

The vendor name had leaked into four layers that outlive any single vendor:

1. **Schema** — `linear_issue_map`, `linear_issue_id` columns (migration 034,
   still unmerged, so renaming in place does not violate "never edit shipped
   migrations")
2. **Config surface** — `MAQUINISTA_LINEAR_*`, `LINEAR_API_KEY`
3. **Core code** — bridge/sync constructed a `LinearClient` and spoke Linear
   column names; `cmd_start.go` wired it by name
4. **Task metadata** — `linear_issue_id` / `linear_url` keys

EX-02+ (role souls, review dispatch, fixer loop, merge mode) are about to
build on the bridge. The seam has to exist before they do, or every later
slice inherits the vendor name.

## Options Considered

| | A. Provider interface + canonical columns in core | B. Adapter package per vendor (core imports interface, providers out-of-package) | C. Keep Linear, wrap later |
|---|---|---|---|
| Vendor swap cost | +1 provider file, +1 factory case | +1 package | schema migration + edits in every layer |
| Core neutrality | no 'linear' outside provider file | same | none |
| Auditability | one file per vendor, in-package, switch in core | indirection across packages | n/a |
| Effort | ≈ 1 day (this PR's revision) | 1–2 days, import-cycle risk with shared Column type | 0 now, permanent debt |

Option A wins on the plan's own landing analysis: one caller, three expected
providers ever — a static `NewProvider` switch is auditable; dynamic plugin
loading (a registry) was rejected as machinery without a customer.

## Decision

**Option A.** Core pipeline code speaks only a `TicketProvider` interface and
a canonical `Column` enum; Linear becomes one provider implementation.

### Shape

```
MAQUINISTA_TICKETS_PROVIDER ─▶ NewProvider(name, key) ─▶ TicketProvider
                                                           ├─ IntakeIssues(ctx, teamID) ([]Issue, error)
                                                           ├─ Columns(ctx, teamID) (map[Column]string, error)
                                                           └─ SetIssueColumn(ctx, issueID, columnID) error
core (bridge.go, sync.go, cmd wiring): TicketProvider + Column only
vendor specifics (GraphQL, auth, envelope): linear.go (+ linear_test.go)
```

- **Canonical columns are the stored values.** `Column` (`In Progress`,
  `In Review`, `Changes Requested`, `Needs Human`, `Done`) is what
  `ticket_issue_map.pending_state` / `last_synced_state` hold; providers map
  canonical → vendor column id via `Columns()`. A provider swap therefore
  never strands stored state. For the MAQ board the canonical set equals
  Linear's column names, so already-written rows stay diff-compatible.
- **Neutral schema.** Migration 034 renamed in place:
  `034_ticket_bridge.sql`, table `ticket_issue_map` (`issue_id` TEXT PK =
  provider issue id, `issue_key`, `team_id`, `task_id` UNIQUE → tasks ON
  DELETE CASCADE, bookkeeping columns). The PK-INSERT-is-the-claim primitive
  is unchanged. `tasks` metadata keys become `ticket_issue_id` / `ticket_url`
  (no rows exist in any deployed DB; alias reads were rejected as permanent
  debt).
- **Neutral env namespace.** `MAQUINISTA_TICKETS_PROVIDER` (default
  `linear`), `MAQUINISTA_TICKETS_API_KEY`, `MAQUINISTA_TICKETS_TEAM_ID`,
  `MAQUINISTA_TICKETS_PROJECT` (fallback `MAQUINISTA_PROJECT`),
  `MAQUINISTA_TICKETS_POLL` (60 s default). Gate: key + team, else logged
  no-op. The legacy `LINEAR_API_KEY` fallback lives **inside** the Linear
  provider as migration sugar — core never reads it.
- **Core neutrality is enforced by negative checks**, not convention:
  vendor-transport symbols (`LINEAR_API_KEY`, the Linear API host,
  `LinearClient`, GraphQL) must match exactly `linear.go` + `linear_test.go`
  under `internal/pipeline/`; the provider name `"linear"` (factory key,
  config default) is configuration data, not coupling. `rg -i linear
  internal/db/ cmd/` must be empty.

### Consequences

- **Positive:** adding Jira/Height/Shortcut is one provider file + one factory
  case; EX-02+ build on the seam, not the vendor; stored state is vendor-free
  by construction; the neutrality rule is mechanically checkable in review.
- **Negative:** one indirection layer (interface + enum + factory) for a
  single live provider; the Linear-specific transport semantics (raw
  Authorization, data-envelope unwrap, GQL error surfacing) now live behind
  tests rather than in plain sight of core readers.
- **Neutral:** ADR-0005 stays as history, untouched; no second provider is
  implemented — the seam is proven by negative checks + interface tests, not
  by a Jira port. Webhook intake stays out (polling, 60 s bound unchanged).

### Revisit Triggers

- A second tracker actually onboards → revisit factory (still a switch) and
  whether canonical columns cover its workflow model.
- A provider needs background resources or slow construction → revisit the
  synchronous `NewProvider` assumption.
- Canonical column set no longer fits a vendor's model (e.g. no "Needs
  Human") → extend `Column` or make provider mapping partial with explicit
  unmapped-skip semantics (already the sync behavior).

## References

- PR #1 review directive (Otavio, 01/10/2026) — the must-fix before merge
- `.specs/features/ticket-abstraction/{plan,checks}.md` — this decision's
  execution spec (AC 1–13, checks C1–C14)
- `internal/pipeline/provider.go` — `TicketProvider`, `Issue`, `Column`,
  `NewProvider`
- `internal/db/migrations/034_ticket_bridge.sql` — neutral schema
- ADR-0005 §Design — the superseded Linear-specific bits
