"use client";

import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

// SSE event shapes. The server emits one event per pg NOTIFY
// channel; payload contains {payload: <notify payload string>}
// plus any augmentation the server added.
type SSEEvent = {
  type:
    | "ready"
    | "agent_inbox_new"
    | "agent_outbox_new"
    | "channel_delivery_new"
    | "agent_stop"
    | "tool_event"
    | "error";
  data: unknown;
};

// ToolEventPayload is the parsed JSON from a "tool_event" pg_notify payload.
export type ToolEventPayload = {
  agent_id: string;
  type: "tool_use" | "tool_result";
  tool_name: string;
  tool_use_id: string;
  is_error: boolean;
};

export type SSEStatus = "connecting" | "open" | "closed";

// useDashStream opens a single EventSource to /api/stream and
// invalidates the relevant TanStack Query caches on each event.
// Exports connection status for a "live" indicator in the UI.
export function useDashStream() {
  const queryClient = useQueryClient();
  const [status, setStatus] = useState<SSEStatus>("connecting");
  const retryRef = useRef(0);

  useEffect(() => {
    // Disable in SSR; the hook is client-only but guard anyway.
    if (typeof window === "undefined") return;

    let cancelled = false;
    let source: EventSource | null = null;

    const connect = () => {
      if (cancelled) return;
      source = new EventSource("/api/stream");
      setStatus("connecting");

      source.addEventListener("open", () => {
        setStatus("open");
        retryRef.current = 0;
      });

      const bump = (type: SSEEvent["type"]) => (ev: MessageEvent) => {
        const payload = (() => {
          try {
            return JSON.parse(ev.data);
          } catch {
            return ev.data;
          }
        })();

        switch (type) {
          case "agent_inbox_new":
            queryClient.invalidateQueries({ queryKey: ["agents"] });
            queryClient.invalidateQueries({ queryKey: ["inbox"] });
            queryClient.invalidateQueries({ queryKey: ["conversation"] });
            break;
          case "agent_outbox_new":
            queryClient.invalidateQueries({ queryKey: ["agents"] });
            queryClient.invalidateQueries({ queryKey: ["outbox"] });
            queryClient.invalidateQueries({ queryKey: ["conversation"] });
            break;
          case "channel_delivery_new":
            queryClient.invalidateQueries({ queryKey: ["outbox"] });
            break;
          case "agent_stop":
            queryClient.invalidateQueries({ queryKey: ["agents"] });
            break;
        }
        // No-op for payload for now; kept for future targeted cache
        // writes (e.g. update a single row in-place).
        void payload;
      };

      source.addEventListener(
        "ready",
        () => {
          setStatus("open");
          retryRef.current = 0;
        },
      );
      source.addEventListener("agent_inbox_new", bump("agent_inbox_new"));
      source.addEventListener("agent_outbox_new", bump("agent_outbox_new"));
      source.addEventListener(
        "channel_delivery_new",
        bump("channel_delivery_new"),
      );
      source.addEventListener("agent_stop", bump("agent_stop"));

      // tool_event: re-dispatch as a DOM CustomEvent so per-agent hooks can
      // subscribe without opening a second SSE connection.
      source.addEventListener("tool_event", (ev: MessageEvent) => {
        try {
          const frame = JSON.parse(ev.data) as { payload: string };
          const detail = JSON.parse(frame.payload) as ToolEventPayload;
          window.dispatchEvent(
            new CustomEvent("maq:tool_event", { detail }),
          );
        } catch {
          /* malformed payload — ignore */
        }
      });

      source.addEventListener("error", () => {
        setStatus("closed");
        source?.close();
        if (cancelled) return;
        const delay = Math.min(30_000, 500 * 2 ** retryRef.current);
        retryRef.current += 1;
        setTimeout(connect, delay);
      });
    };

    connect();

    return () => {
      cancelled = true;
      source?.close();
    };
  }, [queryClient]);

  return status;
}
