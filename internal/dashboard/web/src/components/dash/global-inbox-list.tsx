"use client";

import { useState } from "react";
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import type { GlobalInboxRow } from "@/lib/types";

// InboxRowItem handles one global inbox row with optional inline
// quick-compose. Click the Reply button to expand a textarea without
// navigating away; Esc or Cancel collapses it.
function InboxRowItem({ r }: { r: GlobalInboxRow }) {
  const [composing, setComposing] = useState(false);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const queryClient = useQueryClient();

  const label = r.agent_handle ?? r.agent_id;
  const href = `/agents/${encodeURIComponent(r.agent_id)}?tab=inbox`;

  async function send() {
    if (!text.trim() || busy) return;
    setBusy(true);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(r.agent_id)}/inbox`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ text: text.trim() }),
        },
      );
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      setText("");
      setComposing(false);
      toast.success("sent");
      queryClient.invalidateQueries({ queryKey: ["inbox"] });
    } catch (err) {
      toast.error(
        `send failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <li key={r.id}>
      <Card
        data-testid={`inbox-row-${r.id}`}
        data-agent-id={r.agent_id}
        data-status={r.status}
        className="flex flex-col gap-1 p-3 transition-colors hover:bg-accent/40"
      >
        {/* Header row — link only when not composing */}
        <div className="flex flex-wrap items-center gap-2">
          {composing ? (
            <span className="font-medium">{label}</span>
          ) : (
            <Link
              href={href}
              className="font-medium hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              {label}
            </Link>
          )}
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
          {!composing && (
            <Button
              size="sm"
              variant="ghost"
              className="h-6 px-2 text-xs"
              data-testid={`inbox-row-reply-${r.id}`}
              onClick={() => setComposing(true)}
            >
              Reply
            </Button>
          )}
        </div>

        <p className="line-clamp-2 text-sm text-foreground/80">
          {r.excerpt ?? "<empty>"}
        </p>

        {composing && (
          <div className="mt-2 flex flex-col gap-2">
            <textarea
              data-testid={`inbox-row-compose-${r.id}`}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring resize-none"
              rows={2}
              placeholder={`Reply to ${label}…`}
              value={text}
              autoFocus
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  setComposing(false);
                  setText("");
                }
                if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                  void send();
                }
              }}
              disabled={busy}
            />
            <div className="flex gap-2 justify-end">
              <Button
                size="sm"
                variant="ghost"
                className="text-xs"
                onClick={() => {
                  setComposing(false);
                  setText("");
                }}
                disabled={busy}
              >
                Cancel
              </Button>
              <Button
                size="sm"
                className="text-xs"
                data-testid={`inbox-row-send-${r.id}`}
                onClick={() => void send()}
                disabled={busy || !text.trim()}
              >
                Send
              </Button>
            </div>
          </div>
        )}
      </Card>
    </li>
  );
}

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
        No pending external messages. Agents are caught up.
      </Card>
    );
  }

  return (
    <ul data-testid="inbox-list" className="flex flex-col gap-2">
      {rows.map((r) => (
        <InboxRowItem key={r.id} r={r} />
      ))}
    </ul>
  );
}
