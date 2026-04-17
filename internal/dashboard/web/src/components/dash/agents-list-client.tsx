"use client";

import { AgentCard } from "@/components/dash/agent-card";
import { useAgents } from "@/lib/hooks";
import { useDashStream } from "@/lib/sse";
import type { AgentListItem } from "@/lib/types";

// Client-side wrapper around the Server-Component-rendered agent
// list. Takes the SSR payload as initialData so there's no flicker,
// opens the SSE stream, and re-fetches /api/agents on every
// agent_inbox_new / agent_outbox_new / agent_stop event.
export function AgentsListClient({ initial }: { initial: AgentListItem[] }) {
  const { data = [] } = useAgents(initial);
  const status = useDashStream();

  return (
    <>
      <div
        data-testid="dash-stream-status"
        data-sse-status={status}
        className="sr-only"
      >
        stream: {status}
      </div>
      {data.length === 0 && (
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
        {data.map((a) => (
          <AgentCard key={a.id} agent={a} />
        ))}
      </div>
    </>
  );
}
