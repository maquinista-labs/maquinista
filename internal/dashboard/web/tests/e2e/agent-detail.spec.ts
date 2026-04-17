import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  insertInbox,
  insertOutbox,
  pgUrlFromState,
} from "./support/db";

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

test.describe("agent detail — tabs", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("card tap lands on the detail page", async ({ page }) => {
    await insertAgent({ id: "detail-tap" });
    await insertOutbox("detail-tap", "hello from detail-tap");

    await page.goto("/agents");
    await page.getByTestId("agent-card-detail-tap").click();
    await expect(page).toHaveURL(/\/agents\/detail-tap$/);
    await expect(page.getByTestId("agent-detail-page")).toHaveAttribute(
      "data-agent-id",
      "detail-tap",
    );
  });

  test("missing agent returns 404", async ({ page }) => {
    const res = await page.goto("/agents/does-not-exist");
    expect(res?.status()).toBe(404);
  });

  test("three tabs are visible and switchable", async ({ page }) => {
    await insertAgent({ id: "tabs" });
    await page.goto("/agents/tabs");
    await expect(page.getByTestId("agent-detail-tabs")).toBeVisible();
    await expect(page.getByTestId("tab-conversation")).toBeVisible();
    await expect(page.getByTestId("tab-inbox")).toBeVisible();
    await expect(page.getByTestId("tab-outbox")).toBeVisible();

    await page.getByTestId("tab-inbox").click();
    await expect(page.getByTestId("tab-inbox")).toHaveAttribute(
      "aria-selected",
      "true",
    );

    await page.getByTestId("tab-outbox").click();
    await expect(page.getByTestId("tab-outbox")).toHaveAttribute(
      "aria-selected",
      "true",
    );
  });

  test("inbox tab lists seeded rows and paginates", async ({ page }) => {
    await insertAgent({ id: "paged" });
    // 52 rows — one page of 50 plus a remainder, so Load more
    // should appear.
    for (let i = 0; i < 52; i++) {
      await insertInbox("paged", `inbox #${i}`);
    }

    await page.goto("/agents/paged");
    await page.getByTestId("tab-inbox").click();
    const list = page.getByTestId("inbox-list");
    await expect(list).toBeVisible();
    await expect(list.locator("li")).toHaveCount(50, { timeout: 5000 });

    await page.getByTestId("inbox-load-more").click();
    await expect(list.locator("li")).toHaveCount(52, { timeout: 5000 });
  });

  test("outbox tab renders rows", async ({ page }) => {
    await insertAgent({ id: "outbox-a" });
    await insertOutbox("outbox-a", "first");
    await insertOutbox("outbox-a", "second");

    await page.goto("/agents/outbox-a");
    await page.getByTestId("tab-outbox").click();
    const list = page.getByTestId("outbox-list");
    await expect(list).toBeVisible();
    await expect(list.locator("li")).toHaveCount(2);
    await expect(list).toContainText("first");
    await expect(list).toContainText("second");
  });

  test("conversation tab shows bubbles + live SSE update", async ({
    page,
  }) => {
    await insertAgent({ id: "chatty" });
    await insertInbox("chatty", "you there?");
    await insertOutbox("chatty", "yep, working on it");

    await page.goto("/agents/chatty");
    // conversation is the default tab — no click needed.
    const view = page.getByTestId("conv-view");
    await expect(view).toBeVisible({ timeout: 5000 });
    await expect(view).toContainText("you there?");
    await expect(view).toContainText("yep, working on it");

    // Insert another outbox row while page is live → SSE
    // invalidate → new bubble.
    await insertOutbox("chatty", "just fixed it");
    await expect(view).toContainText("just fixed it", { timeout: 5000 });
  });
});
