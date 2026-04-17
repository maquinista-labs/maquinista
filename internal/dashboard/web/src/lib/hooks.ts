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
