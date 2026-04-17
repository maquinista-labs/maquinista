// Agents page — Server Component. Reads the list from Postgres at
// first paint (zero JS required to see the initial data). Hands
// the SSR payload off to AgentsListClient which attaches TanStack
// Query + SSE for live updates.

import { AgentsListClient } from "@/components/dash/agents-list-client";
import { getPool } from "@/lib/db";
import { listAgents } from "@/lib/queries";
import type { AgentListItem } from "@/lib/types";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function AgentsPage() {
  let agents: AgentListItem[] = [];
  let error: string | null = null;
  try {
    agents = await listAgents(getPool());
  } catch (err) {
    error = err instanceof Error ? err.message : String(err);
  }

  return (
    <section className="mx-auto max-w-screen-sm px-4 py-6">
      <h2 className="mb-3 text-xl font-semibold">Agents</h2>

      {error && (
        <p
          data-testid="agents-error"
          className="rounded border border-destructive/60 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {error}
        </p>
      )}

      {!error && <AgentsListClient initial={agents} />}
    </section>
  );
}
