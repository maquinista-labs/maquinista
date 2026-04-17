// Agent detail page — Server Component. Loads the agent row at
// first paint, renders the three-tab shell below.

import { notFound } from "next/navigation";

import { AgentDetailTabs } from "@/components/dash/agent-detail-tabs";
import { getPool } from "@/lib/db";
import { getAgent } from "@/lib/queries";

export const dynamic = "force-dynamic";
export const revalidate = 0;

export default async function AgentDetailPage(props: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await props.params;

  let notFoundFlag = false;
  let error: string | null = null;
  let agent: Awaited<ReturnType<typeof getAgent>> = null;
  try {
    agent = await getAgent(getPool(), id);
    if (!agent) notFoundFlag = true;
  } catch (err) {
    error = err instanceof Error ? err.message : String(err);
  }

  if (notFoundFlag) notFound();

  return (
    <section
      data-testid="agent-detail-page"
      data-agent-id={id}
      className="mx-auto max-w-screen-sm px-4 py-4"
    >
      <header className="mb-3 flex flex-wrap items-center gap-2">
        <h2 className="text-xl font-semibold">{id}</h2>
        {agent && (
          <span
            data-testid="agent-detail-runner"
            className="text-xs text-muted-foreground"
          >
            {agent.runner}
            {agent.model ? ` · ${agent.model}` : ""}
          </span>
        )}
      </header>

      {error && (
        <p
          data-testid="agent-detail-error"
          className="rounded border border-destructive/60 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {error}
        </p>
      )}

      {!error && <AgentDetailTabs agentId={id} />}
    </section>
  );
}
