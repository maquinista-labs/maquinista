import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  withDb,
} from "./support/db";

// G.2 gate — the top-level /conversations (Chats) feed groups
// mailbox rows by conversation_id, surfaces the latest preview +
// pending_count, and routes to the agent chat tab on tap.

async function seedConversation(args: {
  agentId: string;
  handle?: string | null;
  inboxMessages: Array<{ text: string; status?: string }>;
  outboxMessages: Array<{ text: string }>;
}): Promise<string> {
  const {
    agentId,
    handle = null,
    inboxMessages,
    outboxMessages,
  } = args;
  await insertAgent({ id: agentId, handle });

  let convId = "";
  await withDb(async (c) => {
    const convRes = await c.query(
      `INSERT INTO conversations
         (origin_inbox_id, origin_user_id, origin_thread_id, origin_chat_id)
       VALUES (gen_random_uuid(), 'test-user', '1', 1)
       RETURNING id`,
    );
    convId = convRes.rows[0].id;

    for (const m of inboxMessages) {
      await c.query(
        `INSERT INTO agent_inbox
           (agent_id, conversation_id, from_kind, content, status)
         VALUES ($1, $2, 'user', $3, $4)`,
        [
          agentId,
          convId,
          JSON.stringify({ text: m.text }),
          m.status ?? "pending",
        ],
      );
    }
    for (const m of outboxMessages) {
      await c.query(
        `INSERT INTO agent_outbox (agent_id, conversation_id, content)
         VALUES ($1, $2, $3)`,
        [agentId, convId, JSON.stringify({ text: m.text })],
      );
    }
  });
  return convId;
}

test.describe("global chats feed", () => {
  test.beforeEach(async () => {
    await cleanTables();
  });

  test("shows one row per conversation, newest first", async ({
    page,
  }) => {
    await seedConversation({
      agentId: "agent-old",
      handle: "old",
      inboxMessages: [{ text: "old msg" }],
      outboxMessages: [{ text: "old reply" }],
    });
    // Nudge row apart in time so ORDER BY last_at DESC is
    // deterministic under second-resolution timestamps.
    await new Promise((r) => setTimeout(r, 1100));
    const newer = await seedConversation({
      agentId: "agent-new",
      handle: "new",
      inboxMessages: [{ text: "new pending", status: "pending" }],
      outboxMessages: [{ text: "latest reply" }],
    });

    await page.goto("/conversations");
    const list = page.getByTestId("chats-list");
    await expect(list).toBeVisible();

    const rows = page.locator('[data-testid^="chat-row-"]');
    await expect(rows).toHaveCount(2);

    // Newest conversation renders first.
    await expect(rows.first()).toHaveAttribute("data-agent-id", "agent-new");

    // Pending badge is present only on the conversation with a
    // pending inbox row.
    await expect(page.getByTestId(`chat-pending-${newer}`)).toBeVisible();
  });

  test("row tap navigates to the agent's chat tab with the conversation filter", async ({
    page,
  }) => {
    const cid = await seedConversation({
      agentId: "agent-x",
      handle: "x",
      inboxMessages: [{ text: "hi" }],
      outboxMessages: [{ text: "ack" }],
    });
    await page.goto("/conversations");
    await page.getByTestId(`chat-row-${cid}`).click();
    await expect(page).toHaveURL(
      new RegExp(`/agents/agent-x\\?tab=chat&conversation=${cid}`),
    );
  });

  test("empty state renders when no conversations exist", async ({
    page,
  }) => {
    await page.goto("/conversations");
    await expect(page.getByTestId("chats-empty")).toBeVisible();
  });

  test("single-agent chat without conversation_id falls back to agent_id", async ({
    page,
  }) => {
    // Single-agent Telegram topic chats never populate conversation_id
    // — only multi-agent a2a handoffs do. The feed must still surface
    // them; the fallback grouping uses agent_id as the thread key.
    await insertAgent({ id: "t-1-42", handle: "topic-agent" });
    await withDb(async (c) => {
      await c.query(
        `INSERT INTO agent_inbox (agent_id, from_kind, content, status)
         VALUES ($1, 'user', $2, 'processed')`,
        ["t-1-42", JSON.stringify({ text: "telegram msg" })],
      );
      await c.query(
        `INSERT INTO agent_outbox (agent_id, content)
         VALUES ($1, $2)`,
        ["t-1-42", JSON.stringify({ text: "agent reply" })],
      );
    });

    await page.goto("/conversations");
    const list = page.getByTestId("chats-list");
    await expect(list).toBeVisible();

    const row = page.getByTestId("chat-row-t-1-42");
    await expect(row).toBeVisible();
    // Fallback thread links to the agent's timeline without a
    // `conversation=` query param (there's no real conversation row
    // to filter on).
    await row.click();
    await expect(page).toHaveURL(/\/agents\/t-1-42\?tab=chat$/);
  });
});
