"use client";

import { useEffect, useRef, useState } from "react";
import type { ToolEventPayload } from "@/lib/sse";

// LiveToolCall represents a tool call that is in-flight or recently completed.
export type LiveToolCall = {
  callId: string;
  toolName: string;
  startedAt: number; // Date.now() when tool_use was observed
  elapsedMs: number; // updated every second while running
  status: "running" | "done";
};

// DONE_TTL: how long a completed tool call stays visible before being removed.
const DONE_TTL_MS = 2000;

// useAgentLive subscribes to tool_event DOM events (dispatched by useDashStream)
// and returns the current list of live tool calls for the given agent.
// Elapsed time ticks every second for running calls.
export function useAgentLive(agentId: string): LiveToolCall[] {
  const [calls, setCalls] = useState<Map<string, LiveToolCall>>(new Map());
  // Use a ref so the interval closure always sees current state without
  // needing to be in the effect's deps (avoids resetting the interval).
  const callsRef = useRef(calls);
  callsRef.current = calls;

  // Subscribe to tool events from useDashStream's DOM relay.
  useEffect(() => {
    if (typeof window === "undefined") return;

    const handler = (e: Event) => {
      const detail = (e as CustomEvent<ToolEventPayload>).detail;
      if (detail.agent_id !== agentId) return;

      if (detail.type === "tool_use") {
        const call: LiveToolCall = {
          callId: detail.tool_use_id,
          toolName: detail.tool_name,
          startedAt: Date.now(),
          elapsedMs: 0,
          status: "running",
        };
        setCalls((prev) => new Map(prev).set(detail.tool_use_id, call));
      } else if (detail.type === "tool_result") {
        setCalls((prev) => {
          const next = new Map(prev);
          const existing = next.get(detail.tool_use_id);
          if (existing) {
            next.set(detail.tool_use_id, { ...existing, status: "done" });
          }
          return next;
        });
        // Remove the done entry after TTL.
        setTimeout(() => {
          setCalls((prev) => {
            const next = new Map(prev);
            next.delete(detail.tool_use_id);
            return next;
          });
        }, DONE_TTL_MS);
      }
    };

    window.addEventListener("maq:tool_event", handler);
    return () => window.removeEventListener("maq:tool_event", handler);
  }, [agentId]);

  // Tick elapsed time for running calls every second.
  useEffect(() => {
    const timer = setInterval(() => {
      const current = callsRef.current;
      const hasRunning = [...current.values()].some(
        (c) => c.status === "running",
      );
      if (!hasRunning) return;

      const now = Date.now();
      setCalls((prev) => {
        const next = new Map(prev);
        for (const [k, v] of next) {
          if (v.status === "running") {
            next.set(k, { ...v, elapsedMs: now - v.startedAt });
          }
        }
        return next;
      });
    }, 1000);
    return () => clearInterval(timer);
  }, []);

  return [...calls.values()];
}
