import { describe, expect, it } from "vitest";

import type { AgentListItem } from "@/lib/types";
import { agentStatusDot } from "./agent-card";

function agent(partial: Partial<AgentListItem> = {}): AgentListItem {
  return {
    id: "m",
    handle: null,
    runner: "claude",
    model: null,
    role: "executor",
    status: "working",
    stop_requested: false,
    tmux_window: "0:1",
    started_at: "2026-04-17T09:00:00Z",
    last_seen: "2026-04-17T09:00:00Z",
    last_outbox_excerpt: null,
    last_outbox_at: null,
    unread_inbox_count: 0,
    persona: null,
    ...partial,
  };
}

describe("agentStatusDot", () => {
  const now = Date.parse("2026-04-17T09:00:10Z"); // 10 s after last_seen

  it("is green when working and last_seen < 30 s", () => {
    expect(agentStatusDot(agent(), now)).toBe("green");
  });

  it("is amber when working but last_seen ≥ 30 s", () => {
    expect(agentStatusDot(agent(), now + 60_000)).toBe("amber");
  });

  it("is red when working + stop_requested", () => {
    expect(
      agentStatusDot(agent({ stop_requested: true }), now),
    ).toBe("red");
  });

  it("is red when working but tmux_window is missing", () => {
    expect(agentStatusDot(agent({ tmux_window: null }), now)).toBe(
      "red",
    );
  });

  it("is gray when idle", () => {
    expect(agentStatusDot(agent({ status: "idle" }), now)).toBe("gray");
  });

  it("is gray when archived", () => {
    expect(agentStatusDot(agent({ status: "archived" }), now)).toBe(
      "gray",
    );
  });

  it("is gray when last_seen is null", () => {
    expect(agentStatusDot(agent({ last_seen: null }), now)).toBe(
      "gray",
    );
  });
});
