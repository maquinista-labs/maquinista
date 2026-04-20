"use client";

import { useEffect, useState } from "react";
import type { ToolEventPayload } from "@/lib/sse";

// LiveToolEvent represents a single tool_use or tool_result SSE event.
export type LiveToolEvent = {
  id: string;                        // unique per-event id (tool_use_id + kind)
  toolUseId: string;
  kind: "tool_use" | "tool_result";
  toolName: string;
  toolInput?: string;                // set on tool_use
  text?: string;                     // set on tool_result (truncated result)
  isError: boolean;
  at: Date;
};

// useAgentLive subscribes to tool_event DOM events (dispatched by useDashStream)
// and returns all tool events seen for the given agent since the component
// mounted. Events are never removed — they accumulate until page reload.
export function useAgentLive(agentId: string): LiveToolEvent[] {
  const [events, setEvents] = useState<LiveToolEvent[]>([]);

  useEffect(() => {
    if (typeof window === "undefined") return;

    const handler = (e: Event) => {
      const detail = (e as CustomEvent<ToolEventPayload>).detail;
      if (detail.agent_id !== agentId) return;

      const event: LiveToolEvent = {
        id: `${detail.tool_use_id}-${detail.type}`,
        toolUseId: detail.tool_use_id,
        kind: detail.type,
        toolName: detail.tool_name,
        toolInput: detail.tool_input,
        text: detail.text,
        isError: detail.is_error,
        at: new Date(),
      };

      setEvents((prev) => [...prev, event]);
    };

    window.addEventListener("maq:tool_event", handler);
    return () => window.removeEventListener("maq:tool_event", handler);
  }, [agentId]);

  return events;
}
