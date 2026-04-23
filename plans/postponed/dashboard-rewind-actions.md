# Dashboard write actions: rewind, outbox void/reroute, memory pin

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres
> is the system of record**.

## Context

`plans/active/dashboard.md` Phase 5 shipped the four actions that
have no cross-plan dependencies: composer, interrupt, kill, respawn.
The other four actions from the original plan were deliberately
deferred because each one depends on a feature that doesn't exist
yet:

- **Rewind to checkpoint** — needs the shadow-git checkpoint
  pipeline from `active/checkpoint-rollback.md`.
- **Void outbox row** — needs a `voided` status in the agent_outbox
  check constraint (currently: `pending|routing|routed|failed`).
- **Reroute outbox row to another agent** — needs mention fan-out
  from `active/agent-to-agent-communication.md` (Phase 1 is at 40%).
- **Pin memory** — needs the memory-blocks UI, which depends on
  `active/agent-memory-db.md` Phase 0.

This plan lands the four endpoints + affordances once those
upstream plans ship.

## Scope

Four write endpoints, each in its own commit, each with a Playwright
spec inserted into `tests/e2e/actions.spec.ts` alongside the
existing 7 specs.

### Commit R.1 — Outbox void

Blocked on: none (small migration).

- Migration `027_agent_outbox_void.sql` — alter the status check
  constraint to `('pending','routing','routed','failed','voided')`.
- `src/lib/actions.ts` gain `voidOutbox(pool, id)`:
  `UPDATE agent_outbox SET status='voided' WHERE id=$1 AND status IN ('pending','routing')`.
  Returns the rowCount so 404 on no-op is clean.
- `POST /api/outbox/[id]/void` — calls the helper, 404 on miss,
  audits.
- UI: long-press on an `OutboxList` row opens a confirm sheet with
  Void (destructive). Success toast + SSE re-invalidates outbox
  query.
- Playwright: seed an outbox row with status='pending', tap the
  row's menu, confirm void, assert DB row has status='voided',
  assert the card greys out.

### Commit R.2 — Outbox reroute

Blocked on: `active/agent-to-agent-communication.md` Phase 1
(mention fan-out helper that writes an `agent_inbox` row with
`from_kind='agent'` + the target agent id).

- `rerouteOutbox(pool, outboxId, targetAgent)` — reads content,
  writes a new `agent_inbox` row for target with
  `from_kind='agent'`, `in_reply_to=null`, content copied over.
  Also marks the source outbox row with `status='routed'` so it
  doesn't re-fan-out.
- `POST /api/outbox/[id]/reroute` — accepts `{target_agent}`, 400
  if target is the source agent, 404 if target doesn't exist.
- UI: row menu adds "Reroute to…" which opens a shadcn Command
  palette populated by `useAgents()`. Filter by `a.id !==
  current`. One-tap to pick.
- Playwright: seed two agents + a source outbox row, reroute,
  assert a new inbox row exists for the target with matching
  content and `from_kind='agent'`.

### Commit R.3 — Memory pin

Blocked on: `active/agent-memory-db.md` Phase 0 (memory-blocks
schema + per-agent memory endpoints).

- `pinMemory(pool, id, pinned)` — `UPDATE agent_memories SET
  pinned=$2 WHERE id=$1`.
- `POST /api/memory/[id]/pin` — accepts `{pinned: bool}`, 404 on
  miss.
- UI: on the agent-detail Memory tab (new in Phase 0 of the memory
  plan), each archival row gets a pin toggle. Pin persists to DB
  and surfaces in the agent card's "#pinned=N" badge.
- Playwright: seed an archival memory row, tap pin, assert
  `pinned=TRUE` in DB + the memory row shows a pinned icon.

### Commit R.4 — Rewind to checkpoint

Blocked on: `active/checkpoint-rollback.md` Phase 3 (checkpoint
list endpoint + the rewind write path).

- `POST /api/agents/[id]/rewind` — accepts `{checkpoint_id, mode:
  "soft"|"hard"}`. Shells to the checkpoint-rollback Go helper
  via an internal HTTP call, or (preferred) writes an
  `agent_inbox` row with `content.control="rewind"` + the
  checkpoint ref, and lets the per-agent sidecar
  (`active/per-agent-sidecar.md`) apply it.
- UI: add a 4th tab to `AgentDetailTabs` — `<CheckpointTimeline>`
  from the checkpoint-rollback plan. Each node has a "Rewind here"
  menu that opens a confirm dialog with a soft/hard toggle.
- Playwright: seed an agent with two checkpoints (via
  checkpoint-rollback's test harness), tap rewind on the older
  one, assert the inbox row is enqueued and (once the sidecar is
  wired) the HEAD reflects the rewind target.

## Files

New:

```
internal/db/migrations/027_agent_outbox_void.sql            R.1
internal/dashboard/web/src/app/api/outbox/[id]/void/route.ts R.1
internal/dashboard/web/src/app/api/outbox/[id]/reroute/route.ts R.2
internal/dashboard/web/src/app/api/memory/[id]/pin/route.ts R.3
internal/dashboard/web/src/app/api/agents/[id]/rewind/route.ts R.4
internal/dashboard/web/src/components/dash/outbox-row-menu.tsx R.1/R.2
internal/dashboard/web/src/components/dash/reroute-picker.tsx R.2
internal/dashboard/web/src/components/dash/checkpoint-timeline.tsx R.4
```

Modified:

```
internal/dashboard/web/src/lib/actions.ts                four new helpers
internal/dashboard/web/src/components/dash/outbox-list.tsx  long-press menu
internal/dashboard/web/src/components/dash/agent-detail-tabs.tsx  4th tab
internal/dashboard/web/tests/e2e/actions.spec.ts         four spec blocks
internal/dashboard/web/tests/e2e/support/db.ts           seed helpers
```

## Verification per commit

Matches the bullets at the end of each commit above. CI fails if a
commit lands without its Playwright spec.

## Interaction with other active plans

- `active/checkpoint-rollback.md` — R.4 is the UI surface for its
  Phase 3 rewind primitive.
- `active/agent-memory-db.md` — R.3 is the pin surface for its
  memory-blocks work. The Memory tab itself ships in the memory
  plan, not here; this commit adds only the per-row pin toggle
  hook.
- `active/agent-to-agent-communication.md` — R.2 consumes the
  mention fan-out helper; R.2 and the a2a Phase 1 can land in
  either order as long as both are present before the Playwright
  gate flips green.
- `active/per-agent-sidecar.md` — R.4's ergonomic path
  (inbox row with `content.control="rewind"`) requires the
  sidecar to recognise the control marker. Until sidecar lands,
  R.4 can shell directly to the checkpoint-rollback Go helper
  via a new `maquinista checkpoint rewind` subcommand.

## Open questions

1. **Row-level long-press vs per-row action menu.** shadcn
   `DropdownMenu` on a list row is easy on desktop but fiddly on
   touch. If the long-press UX is flaky, fall back to a small
   vertical-dots button at the right edge of each row (same
   surface `AgentActions` uses on the detail header).
2. **Reroute auth boundary.** Should the operator be able to
   reroute outbox rows to agents outside their scope (once
   multi-tenancy lands)? Ship as no-boundary for v1; revisit
   with `active/productization-saas.md`.
3. **Rewind idempotency.** Two operators tapping Rewind on the
   same checkpoint within the same second — does the sidecar
   dedupe by `(agent_id, checkpoint_id, created_at < 5s)`?
   Specified in checkpoint-rollback plan; this plan inherits.
