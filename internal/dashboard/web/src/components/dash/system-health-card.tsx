"use client";

import { useQuery } from "@tanstack/react-query";

import { Card } from "@/components/ui/card";
import type { SystemHealth } from "@/lib/types";

function humanDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function SystemHealthCard() {
  const q = useQuery<SystemHealth, Error>({
    queryKey: ["health"],
    queryFn: async () => {
      const res = await fetch("/api/health", { cache: "no-store" });
      if (!res.ok) throw new Error(`GET /api/health ${res.status}`);
      return (await res.json()) as SystemHealth;
    },
    staleTime: 5_000,
    refetchOnWindowFocus: true,
  });

  const h = q.data;
  if (!h) return null;

  return (
    <Card
      data-testid="system-health-card"
      className="mt-3 flex flex-wrap gap-x-4 gap-y-1 p-3 text-xs"
    >
      <span data-testid="health-pg">
        pg: {h.pg.total} / idle {h.pg.idle}
        {h.pg.waiting > 0 ? ` / waiting ${h.pg.waiting}` : ""}
      </span>
      <span data-testid="health-uptime">up {humanDuration(h.uptime_ms)}</span>
      <span className="text-muted-foreground">pid {h.pid}</span>
      <span className="text-muted-foreground">node {h.node_version}</span>
      <span className="text-muted-foreground">{h.platform}</span>
    </Card>
  );
}
