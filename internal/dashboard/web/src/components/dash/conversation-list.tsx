"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import type { ConversationRow } from "@/lib/types";

function relativeTime(iso: string, now = Date.now()): string {
  const diff = (now - new Date(iso).getTime()) / 1000;
  if (diff < 5) return "just now";
  if (diff < 60) return `${Math.floor(diff)}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

export function ConversationList({
  initial,
}: {
  initial: ConversationRow[];
}) {
  const q = useQuery<{ items: ConversationRow[] }, Error>({
    queryKey: ["conversations"],
    initialData: { items: initial },
    queryFn: async () => {
      const res = await fetch("/api/conversations", { cache: "no-store" });
      if (!res.ok) throw new Error(`GET /api/conversations ${res.status}`);
      return res.json() as Promise<{ items: ConversationRow[] }>;
    },
    refetchInterval: 5_000,
  });

  const rows = q.data?.items ?? [];

  if (rows.length === 0) {
    return (
      <Card
        data-testid="chats-empty"
        className="p-4 text-sm text-muted-foreground"
      >
        No conversations yet. Reach any agent via Telegram or the
        agent detail composer to start one.
      </Card>
    );
  }

  return (
    <ul data-testid="chats-list" className="flex flex-col gap-2">
      {rows.map((r) => {
        const label = r.agent_handle ?? r.agent_id;
        const href = r.conversation_id
          ? `/agents/${encodeURIComponent(
              r.agent_id,
            )}?tab=chat&conversation=${encodeURIComponent(r.conversation_id)}`
          : `/agents/${encodeURIComponent(r.agent_id)}?tab=chat`;
        const rowKey = r.conversation_id ?? r.thread_key;
        return (
          <li key={`${r.agent_id}-${rowKey}`}>
            <Link
              href={href}
              data-testid={`chat-row-${rowKey}`}
              data-agent-id={r.agent_id}
              className="block focus-visible:outline-none"
            >
              <Card className="flex flex-col gap-1 p-3 transition-colors hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{label}</span>
                  {r.pending_count > 0 && (
                    <Badge
                      data-testid={`chat-pending-${rowKey}`}
                      variant="default"
                    >
                      {r.pending_count}
                    </Badge>
                  )}
                  <span className="ml-auto text-xs text-muted-foreground">
                    {r.msg_count} msg · {relativeTime(r.last_at)}
                  </span>
                </div>
                <p className="line-clamp-2 text-sm text-foreground/80">
                  {r.preview ?? "<empty>"}
                </p>
              </Card>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
