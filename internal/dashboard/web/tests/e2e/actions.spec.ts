import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  pgUrlFromState,
  withDb,
} from "./support/db";

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

test.describe("action surface — write endpoints", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("composer enqueues an inbox row with origin_channel=dashboard", async ({
    page,
  }) => {
    await insertAgent({ id: "talk" });
    await page.goto("/agents/talk");

    const input = page.getByTestId("composer-input");
    await input.click();
    await input.fill("hey, are you there?");
    // Wait for React state to apply and the Send button to enable.
    const send = page.getByTestId("composer-send");
    await expect(send).toBeEnabled();
    await send.click();

    // Wait for the row to land in the DB.
    await expect
      .poll(
        async () => {
          return withDb(async (c) => {
            const { rows } = await c.query(
              `SELECT content->>'text' AS text, origin_channel
               FROM agent_inbox WHERE agent_id='talk'`,
            );
            return rows;
          });
        },
        { timeout: 5000 },
      )
      .toHaveLength(1);

    const inbox = await withDb((c) =>
      c.query(
        `SELECT content->>'text' AS text, origin_channel, status
         FROM agent_inbox WHERE agent_id='talk'`,
      ),
    );
    expect(inbox.rows[0].text).toBe("hey, are you there?");
    expect(inbox.rows[0].origin_channel).toBe("dashboard");
    expect(inbox.rows[0].status).toBe("pending");
  });

  test("quick-reply chip enqueues the preset body", async ({ page }) => {
    await insertAgent({ id: "chips" });
    await page.goto("/agents/chips");

    await page.getByTestId("quickreply-ship-it").click();
    await expect
      .poll(
        async () =>
          withDb(async (c) => {
            const { rows } = await c.query(
              `SELECT content->>'text' AS text FROM agent_inbox WHERE agent_id='chips'`,
            );
            return rows.map((r) => r.text);
          }),
        { timeout: 5000 },
      )
      .toEqual(["ship it"]);
  });

  test("interrupt action writes control=interrupt to inbox", async ({
    page,
  }) => {
    await insertAgent({ id: "interrupt-me" });
    await page.goto("/agents/interrupt-me");

    await page.getByTestId("agent-actions-trigger").click();
    await expect(page.getByTestId("agent-actions-sheet")).toBeVisible();
    await page.getByTestId("action-interrupt").click();

    await expect
      .poll(
        async () =>
          withDb(async (c) => {
            const { rows } = await c.query(
              `SELECT content->>'control' AS control, from_kind
               FROM agent_inbox WHERE agent_id='interrupt-me'`,
            );
            return rows;
          }),
        { timeout: 5000 },
      )
      .toHaveLength(1);

    const rows = (await withDb((c) =>
      c.query(
        `SELECT content->>'control' AS control, from_kind
         FROM agent_inbox WHERE agent_id='interrupt-me'`,
      ),
    )).rows;
    expect(rows[0].control).toBe("interrupt");
    expect(rows[0].from_kind).toBe("system");
  });

  test("kill action flips stop_requested", async ({ page }) => {
    await insertAgent({ id: "kill-me" });
    await page.goto("/agents/kill-me");

    await page.getByTestId("agent-actions-trigger").click();
    await expect(page.getByTestId("agent-actions-sheet")).toBeVisible();
    await page.getByTestId("action-kill").click();

    await expect
      .poll(
        async () =>
          withDb(async (c) => {
            const { rows } = await c.query(
              `SELECT stop_requested FROM agents WHERE id='kill-me'`,
            );
            return rows[0]?.stop_requested;
          }),
        { timeout: 5000 },
      )
      .toBe(true);
  });

  test("respawn clears tmux_window and stop_requested", async ({ page }) => {
    await insertAgent({
      id: "respawn-me",
      stopRequested: true,
      tmuxWindow: "0:7",
    });
    await page.goto("/agents/respawn-me");

    await page.getByTestId("agent-actions-trigger").click();
    await expect(page.getByTestId("agent-actions-sheet")).toBeVisible();
    await page.getByTestId("action-respawn").click();

    await expect
      .poll(
        async () =>
          withDb(async (c) => {
            const { rows } = await c.query(
              `SELECT tmux_window, stop_requested FROM agents WHERE id='respawn-me'`,
            );
            return rows[0];
          }),
        { timeout: 5000 },
      )
      .toEqual({ tmux_window: "", stop_requested: false });
  });

  test("composer rejects empty text", async ({ request }) => {
    await insertAgent({ id: "empty" });
    const res = await request.post("/api/agents/empty/inbox", {
      data: { text: "   " },
    });
    expect(res.status()).toBe(400);
  });

  test("kill on unknown agent returns 404", async ({ request }) => {
    const res = await request.post("/api/agents/__nope__/kill", {});
    expect(res.status()).toBe(404);
  });
});
