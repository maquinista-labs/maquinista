"use client";

import { useQuery } from "@tanstack/react-query";

import type { AgentListItem } from "@/lib/types";

async function fetchAgents(): Promise<AgentListItem[]> {
  const res = await fetch("/api/agents", { cache: "no-store" });
  if (!res.ok) throw new Error(`GET /api/agents ${res.status}`);
  const body = (await res.json()) as { agents: AgentListItem[] };
  return body.agents;
}

export function useAgents(initial?: AgentListItem[]) {
  return useQuery({
    queryKey: ["agents"],
    queryFn: fetchAgents,
    initialData: initial,
    staleTime: 30_000,
  });
}

async function fetchInboxCount(): Promise<number> {
  const res = await fetch("/api/inbox/count", { cache: "no-store" });
  if (!res.ok) throw new Error(`GET /api/inbox/count ${res.status}`);
  const body = (await res.json()) as { count: number };
  return body.count;
}

// useInboxCount returns the total number of pending+processing inbox
// rows across all agents. Query key ["inbox","count"] is invalidated
// automatically by useDashStream when agent_inbox_new fires.
export function useInboxCount() {
  return useQuery({
    queryKey: ["inbox", "count"],
    queryFn: fetchInboxCount,
    staleTime: 15_000,
  });
}
