// Agents page — Server Component. Reads the list from Postgres at
// first paint (zero client JS for the list itself); Client
// Components (Commit 2.6) attach TanStack Query + SSE for live
// updates on top.

import { AgentCard } from "@/components/dash/agent-card";
import { getPool } from "@/lib/db";
import { listAgents } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function AgentsPage() {
  let agents = [] as Awaited<ReturnType<typeof listAgents>>;
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

      {!error && agents.length === 0 && (
        <p
          data-testid="agents-empty"
          className="text-sm text-muted-foreground"
        >
          No agents yet. Start one via <code>./maquinista start</code> — it
          will appear here within a second.
        </p>
      )}

      <div
        data-testid="agents-list"
        className="flex flex-col gap-3"
      >
        {agents.map((a) => (
          <AgentCard key={a.id} agent={a} />
        ))}
      </div>
    </section>
  );
}
