"use client";

import { useEffect, useRef, useState } from "react";
import type { AgentStatusPayload, ToolEventPayload } from "@/lib/sse";

// A merged tool call: starts as "running" when tool_use arrives,
// transitions to "done" or "error" when the paired tool_result arrives.
export type LiveToolCall = {
  id: string;          // tool_use_id
  toolName: string;
  toolInput?: string;
  status: "running" | "done" | "error";
  result?: string;     // set on completion
  startedAt: Date;
  endedAt?: Date;
  elapsedMs?: number;
};

// useAgentLive subscribes to tool_event DOM events and returns a merged
// list of tool calls. tool_use creates a new "running" entry; the paired
// tool_result updates it in-place to "done"/"error".
export function useAgentLive(agentId: string): LiveToolCall[] {
  const [calls, setCalls] = useState<LiveToolCall[]>([]);

  // Tick every 100ms to keep elapsedMs fresh for running calls.
  const tickRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => {
    if (typeof window === "undefined") return;

    const handler = (e: Event) => {
      const detail = (e as CustomEvent<ToolEventPayload>).detail;
      if (detail.agent_id !== agentId) return;

      if (detail.type === "tool_use") {
        const call: LiveToolCall = {
          id: detail.tool_use_id,
          toolName: detail.tool_name,
          toolInput: detail.tool_input,
          status: "running",
          startedAt: new Date(),
        };
        setCalls((prev) => [...prev, call]);
      } else {
        // tool_result — find and update the matching tool_use, or create a done
        // entry directly if no prior tool_use exists (same-cycle paired event).
        const endedAt = new Date();
        setCalls((prev) => {
          const existing = prev.find((c) => c.id === detail.tool_use_id);
          if (existing) {
            return prev.map((c) =>
              c.id === detail.tool_use_id
                ? {
                    ...c,
                    status: detail.is_error ? ("error" as const) : ("done" as const),
                    result: detail.text,
                    endedAt,
                    elapsedMs: endedAt.getTime() - c.startedAt.getTime(),
                  }
                : c,
            );
          }
          // No prior tool_use entry — create a done entry directly.
          const done: LiveToolCall = {
            id: detail.tool_use_id,
            toolName: detail.tool_name,
            toolInput: detail.tool_input,
            status: detail.is_error ? "error" : "done",
            result: detail.text,
            startedAt: endedAt,
            endedAt,
            elapsedMs: 0,
          };
          return [...prev, done];
        });
      }
    };

    window.addEventListener("maq:tool_event", handler);
    return () => window.removeEventListener("maq:tool_event", handler);
  }, [agentId]);

  // Keep elapsed time ticking for running calls.
  useEffect(() => {
    const hasRunning = calls.some((c) => c.status === "running");
    if (hasRunning && !tickRef.current) {
      tickRef.current = setInterval(() => {
        const now = Date.now();
        setCalls((prev) =>
          prev.map((c) =>
            c.status === "running"
              ? { ...c, elapsedMs: now - c.startedAt.getTime() }
              : c,
          ),
        );
      }, 100);
    } else if (!hasRunning && tickRef.current) {
      clearInterval(tickRef.current);
      tickRef.current = null;
    }
    return () => {
      if (tickRef.current) {
        clearInterval(tickRef.current);
        tickRef.current = null;
      }
    };
  }, [calls]);

  return calls;
}

// useAgentStatus subscribes to agent_status CustomEvents and returns the current
// spinner text for the given agent ("" when idle).
export function useAgentStatus(agentId: string): string {
  const [status, setStatus] = useState("");

  useEffect(() => {
    if (typeof window === "undefined" || !agentId) return;

    const handler = (e: Event) => {
      const detail = (e as CustomEvent<AgentStatusPayload>).detail;
      if (detail.agent_id !== agentId) return;
      setStatus(detail.text);
    };

    window.addEventListener("maq:agent_status", handler);
    return () => window.removeEventListener("maq:agent_status", handler);
  }, [agentId]);

  return status;
}

export function formatElapsed(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  return s < 60 ? `${s.toFixed(1)}s` : `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
}
