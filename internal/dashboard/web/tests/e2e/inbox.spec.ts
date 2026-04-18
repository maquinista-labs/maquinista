import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  insertInbox,
  withDb,
} from "./support/db";

// G.1 gate — the top-level /inbox feed is a flat cross-agent view
// of pending+processing messages. Proves the page reads live DB
// state, hides non-pending rows, and navigates into the agent
// detail on tap.

test.describe("global inbox feed", () => {
  test.beforeEach(async () => {
    await cleanTables();
  });

  test("lists only pending+processing rows across agents", async ({
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
    await expect(rows).toHaveCount(2);

    await expect(page.getByText("alpha pending one")).toBeVisible();
    await expect(page.getByText("bravo pending two")).toBeVisible();
    await expect(page.getByText("bravo done")).toHaveCount(0);
  });

  test("empty state renders when nothing is pending", async ({ page }) => {
    await insertAgent({ id: "lonely", handle: "lonely" });
    await page.goto("/inbox");
    await expect(page.getByTestId("inbox-empty")).toBeVisible();
    await expect(page.getByTestId("inbox-empty")).toContainText(
      "No pending or processing messages",
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
