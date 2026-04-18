"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import type { GlobalInboxRow } from "@/lib/types";

export function GlobalInboxList({
  initial,
}: {
  initial: GlobalInboxRow[];
}) {
  const q = useQuery<{ items: GlobalInboxRow[] }, Error>({
    queryKey: ["inbox", "global"],
    initialData: { items: initial },
    queryFn: async () => {
      const res = await fetch("/api/inbox", { cache: "no-store" });
      if (!res.ok) throw new Error(`GET /api/inbox ${res.status}`);
      return res.json() as Promise<{ items: GlobalInboxRow[] }>;
    },
    refetchInterval: 5_000,
  });

  const rows = q.data?.items ?? [];

  if (rows.length === 0) {
    return (
      <Card
        data-testid="inbox-empty"
        className="p-4 text-sm text-muted-foreground"
      >
        No messages yet.
      </Card>
    );
  }

  return (
    <ul data-testid="inbox-list" className="flex flex-col gap-2">
      {rows.map((r) => {
        const label = r.agent_handle ?? r.agent_id;
        const href = `/agents/${encodeURIComponent(r.agent_id)}?tab=inbox`;
        return (
          <li key={r.id}>
            <Link
              href={href}
              data-testid={`inbox-row-${r.id}`}
              data-agent-id={r.agent_id}
              data-status={r.status}
              className="block focus-visible:outline-none"
            >
              <Card className="flex flex-col gap-1 p-3 transition-colors hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{label}</span>
                  <Badge variant="secondary" className="uppercase text-xs">
                    {r.from_kind}
                  </Badge>
                  <Badge variant="outline" className="text-xs">
                    {r.status}
                  </Badge>
                  {r.origin_channel && (
                    <span className="text-xs text-muted-foreground">
                      {r.origin_channel}
                    </span>
                  )}
                  <time className="ml-auto text-xs text-muted-foreground">
                    {new Date(r.enqueued_at).toLocaleTimeString()}
                  </time>
                </div>
                <p className="line-clamp-2 text-sm text-foreground/80">
                  {r.excerpt ?? "<empty>"}
                </p>
              </Card>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
