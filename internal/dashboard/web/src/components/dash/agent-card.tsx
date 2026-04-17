"use client";

import Link from "next/link";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import type { AgentListItem } from "@/lib/types";

// Status dot palette from plans/active/dashboard.md Phase 2:
//   green — status='working' AND last_seen < 30 s
//   amber — status='working' AND last_seen ≥ 30 s
//   red   — status='working' AND (stop_requested OR missing tmux_window)
//   gray  — status IN ('stopped','archived','dead') OR never seen
export type DotColor = "green" | "amber" | "red" | "gray";

export function agentStatusDot(a: AgentListItem, now = Date.now()): DotColor {
  if (
    a.status === "archived" ||
    a.status === "dead" ||
    a.status === "idle" ||
    !a.last_seen
  ) {
    return "gray";
  }
  if (a.stop_requested || !a.tmux_window) return "red";
  const seenMs = new Date(a.last_seen).getTime();
  const ageSec = (now - seenMs) / 1000;
  return ageSec < 30 ? "green" : "amber";
}

function dotClass(c: DotColor) {
  return {
    green: "bg-emerald-500",
    amber: "bg-amber-500",
    red: "bg-red-500",
    gray: "bg-zinc-500",
  }[c];
}

function relativeTime(iso: string | null, now = Date.now()): string {
  if (!iso) return "never";
  const diff = (now - new Date(iso).getTime()) / 1000;
  if (diff < 5) return "just now";
  if (diff < 60) return `${Math.floor(diff)}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

export function AgentCard({ agent }: { agent: AgentListItem }) {
  const dot = agentStatusDot(agent);
  return (
    <Link
      href={`/agents/${encodeURIComponent(agent.id)}`}
      data-testid={`agent-card-${agent.id}`}
      data-status={agent.status}
      data-dot={dot}
      className="block focus-visible:outline-none"
    >
      <Card className="relative flex flex-col gap-2 p-4 transition-colors hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring">
        <div className="flex items-center gap-2">
          <span
            aria-hidden
            data-testid="agent-status-dot"
            className={cn(
              "inline-block h-2.5 w-2.5 shrink-0 rounded-full",
              dotClass(dot),
            )}
          />
          <span className="font-medium">{agent.id}</span>
          <span className="ml-auto text-xs text-muted-foreground">
            {agent.runner}
            {agent.model ? ` · ${agent.model}` : ""}
          </span>
          {agent.unread_inbox_count > 0 && (
            <Badge
              data-testid="agent-unread-badge"
              variant="default"
              className="ml-1"
            >
              {agent.unread_inbox_count}
            </Badge>
          )}
        </div>

        <div className="text-xs text-muted-foreground">
          last seen {relativeTime(agent.last_seen)}
        </div>

        {agent.last_outbox_excerpt && (
          <p
            data-testid="agent-last-outbox"
            className="line-clamp-2 text-sm text-foreground/80"
          >
            “{agent.last_outbox_excerpt}”
          </p>
        )}

        <div className="flex flex-wrap gap-1 pt-1">
          <Badge variant="secondary">#{agent.role}</Badge>
          {agent.persona && <Badge variant="outline">#{agent.persona}</Badge>}
        </div>
      </Card>
    </Link>
  );
}
