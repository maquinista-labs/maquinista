# Dashboard cost + KPI live updates via NOTIFY

> This plan adheres to §0 of `reference/maquinista-v2.md`: **Postgres
> is the system of record**.

## Context

`plans/active/dashboard.md` Phase 4 shipped the KPI strip + cost
donut + system health card. The KPI strip uses TanStack Query with
a 30-second `refetchInterval` instead of SSE invalidation, because
`agent_turn_costs` and `scheduled_jobs` don't emit `pg_notify`
today — only the mailbox tables do (migration 009).

The operator watching a just-completed turn wants the tile to tick
immediately, not 30 s later. This plan wires the missing
`NOTIFY` triggers + folds the relevant channels into
`/api/stream` so the dashboard gets a sub-second refresh across
costs, job runs, and system health.

## Scope

### Commit C.1 — Cost NOTIFY trigger

Migration `029_agent_turn_costs_notify.sql`:

```sql
CREATE OR REPLACE FUNCTION notify_agent_turn_cost()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('agent_turn_cost_new', NEW.agent_id);
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS on_agent_turn_cost_notify ON agent_turn_costs;
CREATE TRIGGER on_agent_turn_cost_notify
    AFTER INSERT ON agent_turn_costs
    FOR EACH ROW EXECUTE FUNCTION notify_agent_turn_cost();
```

Go unit test (`internal/monitor/cost_test.go`): after
`CaptureTurn`, `LISTEN agent_turn_cost_new` receives the payload
within 1 s.

### Commit C.2 — Jobs NOTIFY trigger

Migration `030_scheduled_jobs_notify.sql` — same pattern against
`scheduled_jobs` + `webhook_handlers`, channels
`scheduled_jobs_change` and `webhook_handlers_change`. Fires on
`INSERT OR UPDATE OR DELETE` so toggles, reschedules, and deletes
all push.

### Commit C.3 — Subscribe in `/api/stream`

Extend `src/app/api/stream/route.ts`'s LISTEN set:

```ts
const CHANNELS = [
  "agent_inbox_new",
  "agent_outbox_new",
  "channel_delivery_new",
  "agent_stop",
  "agent_turn_cost_new",         // new
  "scheduled_jobs_change",       // new
  "webhook_handlers_change",     // new
] as const;
```

Emit each as its own SSE event type.

### Commit C.4 — Invalidate in `useDashStream`

`src/lib/sse.ts` maps the new channels to query invalidations:

```ts
case "agent_turn_cost_new":
  queryClient.invalidateQueries({ queryKey: ["kpis"] });
  break;
case "scheduled_jobs_change":
case "webhook_handlers_change":
  queryClient.invalidateQueries({ queryKey: ["jobs"] });
  break;
```

Also drop the 30 s `refetchInterval` on the `kpis` query — SSE
is now the source of truth; polling would double-fetch.

### Commit C.5 — System health ticks via a tiny heartbeat channel

System health doesn't have a backing table. Instead of adding one,
the Next server self-publishes a heartbeat:

```ts
// src/app/api/stream/route.ts — inside the stream:
const heartbeat = setInterval(() => {
  safeEnqueue(encoder.encode(encodeSSE({
    event: "dash.health_tick",
    data: { at: Date.now() },
  })));
}, 5_000);
```

`useDashStream` invalidates `["health"]` on the tick, replacing
the current 5 s `refetchInterval`. Same cadence, but the client
now only pulls when the server actually has something to say, and
the existing keepalive doubles as the heartbeat.

### Commit C.6 — Playwright spec: KPIs update without polling

Addition to `tests/e2e/kpis-jobs-health.spec.ts`:

```ts
test("KPI tile ticks within 2 s of a turn insertion", async ({ page }) => {
  await insertAgent({ id: "cost-live" });
  await page.goto("/agents");
  const tile = page.getByTestId("kpi-cost-today");
  await expect(tile).toContainText("$0");

  await insertTurnCost({
    agentId: "cost-live",
    model: "claude-sonnet-4-6",
    inputUsdCents: 100,
    outputUsdCents: 200,
  });

  await expect(tile).toContainText("$3", { timeout: 2000 });
});
```

Spec is gated behind `MAQUINISTA_DASHBOARD_PG_URL` like the other
Phase 4 specs.

### Commit C.7 — Remove polling fallback config

Sweep the `refetchInterval` options out of `useKpis` / `useJobs` /
`useSystemHealth` now that SSE covers them. Keep a small
`refetchOnWindowFocus: true` for health only, so returning from a
backgrounded tab still gets a quick refresh.

## Files

New:

```
internal/db/migrations/029_agent_turn_costs_notify.sql     C.1
internal/db/migrations/030_scheduled_jobs_notify.sql       C.2
```

Modified:

```
internal/dashboard/web/src/app/api/stream/route.ts        C.3 + C.5
internal/dashboard/web/src/lib/sse.ts                     C.4
internal/dashboard/web/src/components/dash/kpi-strip.tsx  C.7
internal/dashboard/web/src/components/dash/jobs-page-client.tsx C.7
internal/dashboard/web/src/components/dash/system-health-card.tsx C.7
internal/dashboard/web/tests/e2e/kpis-jobs-health.spec.ts  C.6
internal/monitor/cost_test.go                              C.1
```

## Verification per commit

- C.1 / C.2: Go test listens on the channel and asserts the
  payload after an INSERT / UPDATE.
- C.3: `curl -N http://127.0.0.1:8900/api/stream` + insert a row
  from psql; observe the new event type in the stream.
- C.4: Playwright spec from C.6 green.
- C.5: `kpis-jobs-health.spec.ts` `system health card reports pool
  stats` still green after the refetchInterval removal.
- C.7: Lighthouse "Avoid long main-thread tasks" doesn't regress
  on the agents page (was: polling every 30 s + 5 s; now: one SSE
  connection + no timer).

## Interaction with other active plans

- `active/per-agent-sidecar.md` — Sidecar's cost capture writes
  `agent_turn_costs` rows, which then NOTIFY. Orders-of-magnitude
  more rows per day than today's once-per-inbox pattern; the
  per-client fanout in `/api/stream` needs to stay cheap (the
  existing backpressure + drop-oldest applies).
- `active/productization-saas.md` — multi-tenant scope means the
  NOTIFY payload should include the tenant id eventually, and
  `/api/stream` should filter. Out of scope for this plan;
  revisit when the SaaS plan picks up the dashboard.

## Open questions

1. **Pin the heartbeat cadence.** 5 s is the current refetch
   interval. Drop to 10 s now that SSE carries the bulk of the
   signal? Start with 5 s to match operator muscle memory;
   revisit if the bundle's egress is measurable.
2. **Should cost donut slices be incremental?** An
   `agent_turn_cost_new` NOTIFY gives us `(agent_id, cents)`; we
   could append to the donut without a full re-query. Nice
   optimisation; defer unless the round-trip cost becomes visible.
3. **System-health heartbeat duplication with SSE keepalive.**
   Right now the keepalive emits `: keepalive\n\n` every 15 s and
   the proposed heartbeat emits every 5 s. Merge them? Keep
   separate for clarity — keepalive is invisible to clients,
   heartbeat is a semantic event.
