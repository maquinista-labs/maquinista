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

test.describe("agents list — live data", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("renders a seeded agent card with status dot and excerpt", async ({
    page,
  }) => {
    await insertAgent({ id: "alpha", persona: "planner" });
    await insertOutbox("alpha", "Fixed the webhook dedup bug");

    await page.goto("/agents");
    const card = page.getByTestId("agent-card-alpha");
    await expect(card).toBeVisible();
    await expect(card).toContainText("alpha");
    await expect(card).toContainText("claude");
    await expect(card).toContainText("Fixed the webhook dedup bug");
    await expect(card).toContainText("#planner");
    await expect(page.getByTestId("agent-status-dot").first()).toHaveClass(
      /bg-emerald-500/,
    );
  });

  test("second agent appears without reload (SSE invalidation)", async ({
    page,
  }) => {
    await insertAgent({ id: "one" });
    await page.goto("/agents");
    await expect(page.getByTestId("agent-card-one")).toBeVisible();
    await expect(page.getByTestId("dash-stream-status")).toHaveAttribute(
      "data-sse-status",
      "open",
      { timeout: 5000 },
    );

    await insertAgent({ id: "two" });
    // Inserting an outbox row fires agent_outbox_new → SSE →
    // invalidate agents → refetch.
    await insertOutbox("two", "hello from two");

    await expect(page.getByTestId("agent-card-two")).toBeVisible({
      timeout: 5000,
    });
    await expect(page.getByTestId("agent-card-two")).toContainText(
      "hello from two",
    );
  });

  test("unread inbox badge reflects pending rows", async ({ page }) => {
    await insertAgent({ id: "inboxy" });
    await insertInbox("inboxy", "please help");
    await insertInbox("inboxy", "still waiting");

    await page.goto("/agents");
    const card = page.getByTestId("agent-card-inboxy");
    await expect(card).toBeVisible();
    await expect(card.getByTestId("agent-unread-badge")).toContainText("2");
  });

  test("stop_requested agent shows red status dot", async ({ page }) => {
    await insertAgent({ id: "angry", stopRequested: true });
    await page.goto("/agents");
    const card = page.getByTestId("agent-card-angry");
    await expect(card).toBeVisible();
    await expect(card).toHaveAttribute("data-dot", "red");
  });

  test("amber when last_seen is older than 30 s", async ({ page }) => {
    const oldSeen = new Date(Date.now() - 60_000);
    await insertAgent({ id: "sleepy", lastSeen: oldSeen });
    await page.goto("/agents");
    const card = page.getByTestId("agent-card-sleepy");
    await expect(card).toBeVisible();
    await expect(card).toHaveAttribute("data-dot", "amber");
  });
});
