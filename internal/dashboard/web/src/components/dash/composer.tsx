"use client";

import { useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";

// Composer — sticky bottom bar on agent detail views. Enqueues the
// text as an agent_inbox row via POST /api/agents/<id>/inbox.
// Quick-reply presets are rendered as chips above the input.
export function Composer({
  agentId,
  quickReplies = ["ship it", "try again", "stop"],
}: {
  agentId: string;
  quickReplies?: string[];
}) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const queryClient = useQueryClient();
  const inputRef = useRef<HTMLInputElement | null>(null);

  async function submit(payload: string) {
    if (!payload.trim() || busy) return;
    setBusy(true);
    try {
      const res = await fetch(
        `/api/agents/${encodeURIComponent(agentId)}/inbox`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ text: payload.trim() }),
        },
      );
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `HTTP ${res.status}`);
      }
      setText("");
      toast.success("sent");
      // Optimistic: invalidate relevant queries so the row shows up
      // even if SSE is slow to fire (local fallback).
      queryClient.invalidateQueries({ queryKey: ["inbox", agentId] });
      queryClient.invalidateQueries({
        queryKey: ["conversation", "agent", agentId],
      });
    } catch (err) {
      toast.error(
        `send failed: ${err instanceof Error ? err.message : String(err)}`,
      );
    } finally {
      setBusy(false);
      inputRef.current?.focus();
    }
  }

  return (
    <div
      data-testid="composer"
      className="sticky bottom-0 border-t border-border/60 bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/80"
      style={{ paddingBottom: "env(safe-area-inset-bottom)" }}
    >
      {quickReplies.length > 0 && (
        <div
          data-testid="composer-quickreplies"
          className="flex gap-1 overflow-x-auto px-2 pt-2 scrollbar-hide"
        >
          {quickReplies.map((qr) => (
            <Button
              key={qr}
              data-testid={`quickreply-${qr.replace(/\s+/g, "-")}`}
              size="sm"
              variant="outline"
              className="shrink-0 text-xs"
              onClick={() => submit(qr)}
              disabled={busy}
            >
              {qr}
            </Button>
          ))}
        </div>
      )}
      <form
        className="flex items-center gap-2 p-2"
        onSubmit={(e) => {
          e.preventDefault();
          void submit(text);
        }}
      >
        <input
          ref={inputRef}
          data-testid="composer-input"
          className="flex-1 rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring"
          placeholder={`Message ${agentId}`}
          value={text}
          onChange={(e) => setText(e.target.value)}
          enterKeyHint="send"
          autoComplete="off"
          disabled={busy}
        />
        <Button
          type="submit"
          data-testid="composer-send"
          disabled={busy || !text.trim()}
          size="sm"
        >
          Send
        </Button>
      </form>
    </div>
  );
}
