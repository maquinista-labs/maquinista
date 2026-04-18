import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  insertInbox,
  withDb,
} from "./support/db";

// G.1 gate — the top-level /inbox feed is a flat cross-agent view
// of recent inbox activity (all statuses by default). Proves the
// page reads live DB state, surfaces status badges per row, and
// navigates into the agent detail on tap.

test.describe("global inbox feed", () => {
  test.beforeEach(async () => {
    await cleanTables();
  });

  test("lists recent rows across agents regardless of status", async ({
    page,
  }) => {
    await insertAgent({ id: "a1", handle: "alpha" });
    await insertAgent({ id: "a2", handle: "bravo" });
    await insertInbox("a1", "alpha pending one");
    const done = await insertInbox("a2", "bravo done");
    await withDb((c) =>
      c.query(`UPDATE agent_inbox SET status = 'processed' WHERE id = $1`, [
        done,
      ]),
    );
    await insertInbox("a2", "bravo pending two");

    await page.goto("/inbox");

    const list = page.getByTestId("inbox-list");
    await expect(list).toBeVisible();

    const rows = page.locator('[data-testid^="inbox-row-"]');
    // All three rows are visible now — the processed one too.
    // Operator can see mailbox history, not just a queue.
    await expect(rows).toHaveCount(3);

    await expect(page.getByText("alpha pending one")).toBeVisible();
    await expect(page.getByText("bravo pending two")).toBeVisible();
    await expect(page.getByText("bravo done")).toBeVisible();
  });

  test("narrows to pending+processing when ?status= is passed", async ({
    request,
  }) => {
    await insertAgent({ id: "f1", handle: "filter" });
    await insertInbox("f1", "still pending");
    const done = await insertInbox("f1", "already done");
    await withDb((c) =>
      c.query(`UPDATE agent_inbox SET status = 'processed' WHERE id = $1`, [
        done,
      ]),
    );

    const res = await request.get("/api/inbox?status=pending,processing");
    expect(res.ok()).toBe(true);
    const body = await res.json();
    const texts = body.items.map((r: { excerpt: string | null }) => r.excerpt);
    expect(texts).toContain("still pending");
    expect(texts).not.toContain("already done");
  });

  test("empty state renders when there's no inbox activity at all", async ({
    page,
  }) => {
    await insertAgent({ id: "lonely", handle: "lonely" });
    await page.goto("/inbox");
    await expect(page.getByTestId("inbox-empty")).toBeVisible();
    await expect(page.getByTestId("inbox-empty")).toContainText(
      "No messages yet",
    );
  });

  test("row tap navigates to the owning agent", async ({ page }) => {
    await insertAgent({ id: "t-1-1", handle: "coder" });
    await insertInbox("t-1-1", "please refactor");
    await page.goto("/inbox");
    const row = page.locator('[data-testid^="inbox-row-"]').first();
    await expect(row).toBeVisible();
    await row.click();
    await expect(page).toHaveURL(/\/agents\/t-1-1\?tab=inbox/);
  });
});
