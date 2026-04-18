import { describe, expect, it, vi } from "vitest";

import {
  excerptFromContent,
  listAgents,
  listConversations,
  listGlobalInbox,
  listInbox,
  listOutbox,
} from "./queries";

describe("excerptFromContent", () => {
  it("returns null for null / undefined", () => {
    expect(excerptFromContent(null)).toBeNull();
    expect(excerptFromContent(undefined)).toBeNull();
  });

  it("truncates long strings with ellipsis", () => {
    const long = "x".repeat(200);
    const got = excerptFromContent(long, 50);
    expect(got?.length).toBe(50);
    expect(got?.endsWith("…")).toBe(true);
  });

  it("prefers .text then .body then .message", () => {
    expect(excerptFromContent({ text: "hi" })).toBe("hi");
    expect(excerptFromContent({ body: "hey" })).toBe("hey");
    expect(excerptFromContent({ message: "yo" })).toBe("yo");
  });

  it("falls back to JSON.stringify for unknown shapes", () => {
    expect(excerptFromContent({ weird: 42 })).toContain("weird");
  });
});

function mockPool(rowsByCall: unknown[][]) {
  let call = 0;
  return {
    query: vi.fn(async () => {
      const rows = rowsByCall[call++] ?? [];
      return { rows };
    }),
  } as unknown as import("pg").Pool;
}

describe("listAgents", () => {
  it("maps db rows to AgentListItem with ISO dates + excerpt", async () => {
    const now = new Date("2026-04-17T10:00:00Z");
    const pool = mockPool([
      [
        {
          id: "main",
          handle: "@main",
          runner: "claude",
          model: null,
          role: "executor",
          status: "idle",
          stop_requested: false,
          tmux_window: "0:1",
          started_at: now,
          last_seen: now,
          last_outbox_content: { text: "Fixed the dedup" },
          last_outbox_at: now,
          unread_inbox_count: 2,
          persona: "planner",
        },
      ],
    ]);
    const agents = await listAgents(pool);
    expect(agents).toHaveLength(1);
    expect(agents[0]).toMatchObject({
      id: "main",
      handle: "@main",
      runner: "claude",
      role: "executor",
      status: "idle",
      stop_requested: false,
      last_outbox_excerpt: "Fixed the dedup",
      unread_inbox_count: 2,
      persona: "planner",
    });
    expect(agents[0].started_at).toBe(now.toISOString());
    expect(agents[0].last_outbox_at).toBe(now.toISOString());
  });

  it("handles null last_seen and absent outbox", async () => {
    const now = new Date("2026-04-17T10:00:00Z");
    const pool = mockPool([
      [
        {
          id: "m",
          handle: null,
          runner: "claude",
          model: null,
          role: "executor",
          status: "idle",
          stop_requested: false,
          tmux_window: null,
          started_at: now,
          last_seen: null,
          last_outbox_content: null,
          last_outbox_at: null,
          unread_inbox_count: 0,
          persona: null,
        },
      ],
    ]);
    const agents = await listAgents(pool);
    expect(agents[0].last_seen).toBeNull();
    expect(agents[0].last_outbox_excerpt).toBeNull();
    expect(agents[0].last_outbox_at).toBeNull();
  });
});

describe("listInbox", () => {
  it("enforces limit ≤ 200 and orders DESC", async () => {
    const pool = mockPool([[]]);
    await listInbox(pool, "main", { limit: 9999 });
    const args = (pool.query as unknown as { mock: { calls: unknown[][] } })
      .mock.calls[0];
    const sql = String(args[0]);
    expect(sql).toContain("LIMIT 200");
    expect(sql).toContain("ORDER BY enqueued_at DESC");
  });

  it("adds a cursor predicate when `before` is set", async () => {
    const pool = mockPool([[]]);
    await listInbox(pool, "main", { before: "2026-04-17T00:00:00Z" });
    const args = (pool.query as unknown as { mock: { calls: unknown[][] } })
      .mock.calls[0];
    const sql = String(args[0]);
    const params = args[1] as unknown[];
    expect(sql).toContain("enqueued_at < $2");
    expect(params[1]).toBe("2026-04-17T00:00:00Z");
  });
});

describe("listGlobalInbox", () => {
  it("defaults to pending+processing, caps limit at 200", async () => {
    const pool = mockPool([[]]);
    await listGlobalInbox(pool, { limit: 9999 });
    const args = (pool.query as unknown as { mock: { calls: unknown[][] } })
      .mock.calls[0];
    const sql = String(args[0]);
    const params = args[1] as unknown[];
    expect(sql).toContain("LIMIT 200");
    expect(sql).toContain("JOIN agents");
    expect(sql).toContain("ORDER BY i.enqueued_at DESC");
    expect(params[0]).toEqual(["pending", "processing"]);
  });

  it("accepts a custom status filter", async () => {
    const pool = mockPool([[]]);
    await listGlobalInbox(pool, { status: ["failed", "dead"] });
    const args = (pool.query as unknown as { mock: { calls: unknown[][] } })
      .mock.calls[0];
    const params = args[1] as unknown[];
    expect(params[0]).toEqual(["failed", "dead"]);
  });

  it("maps rows with agent_handle and excerpt", async () => {
    const now = new Date("2026-04-18T10:00:00Z");
    const pool = mockPool([
      [
        {
          id: "i1",
          agent_id: "a1",
          agent_handle: "coder",
          from_kind: "user",
          from_id: null,
          status: "pending",
          origin_channel: "telegram",
          origin_user_id: "42",
          content: { text: "Please fix the dedup bug" },
          enqueued_at: now,
        },
      ],
    ]);
    const items = await listGlobalInbox(pool);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      id: "i1",
      agent_id: "a1",
      agent_handle: "coder",
      excerpt: "Please fix the dedup bug",
      enqueued_at: now.toISOString(),
    });
  });
});

describe("listConversations", () => {
  it("issues one query with a CTE merging inbox + outbox", async () => {
    const pool = mockPool([[]]);
    await listConversations(pool);
    const args = (pool.query as unknown as { mock: { calls: unknown[][] } })
      .mock.calls[0];
    const sql = String(args[0]);
    expect(sql).toContain("WITH last_msg AS");
    expect(sql).toContain("FROM agent_inbox");
    expect(sql).toContain("FROM agent_outbox");
    expect(sql).toContain("ORDER BY lm.last_at DESC");
    expect(sql).toContain("LIMIT 50");
  });

  it("caps limit at 200", async () => {
    const pool = mockPool([[]]);
    await listConversations(pool, 9999);
    const sql = String(
      (pool.query as unknown as { mock: { calls: unknown[][] } }).mock
        .calls[0][0],
    );
    expect(sql).toContain("LIMIT 200");
  });

  it("maps rows with preview excerpt + pending_count", async () => {
    const now = new Date("2026-04-18T10:00:00Z");
    const pool = mockPool([
      [
        {
          conversation_id: "c1",
          agent_id: "a1",
          agent_handle: null,
          last_at: now,
          preview: { text: "Here's the plan" },
          msg_count: 7,
          pending_count: 2,
        },
      ],
    ]);
    const rows = await listConversations(pool);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      conversation_id: "c1",
      agent_id: "a1",
      agent_handle: null,
      preview: "Here's the plan",
      msg_count: 7,
      pending_count: 2,
    });
    expect(rows[0].last_at).toBe(now.toISOString());
  });
});

describe("listOutbox", () => {
  it("uses created_at column + cursor shape", async () => {
    const pool = mockPool([[]]);
    await listOutbox(pool, "main", { before: "2026-04-17T00:00:00Z" });
    const args = (pool.query as unknown as { mock: { calls: unknown[][] } })
      .mock.calls[0];
    const sql = String(args[0]);
    expect(sql).toContain("created_at < $2");
    expect(sql).toContain("ORDER BY created_at DESC");
  });
});
