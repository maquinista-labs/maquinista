import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  insertInbox,
  insertScheduledJob,
  insertTurnCost,
  insertWebhookHandler,
  pgUrlFromState,
} from "./support/db";

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

test.describe("KPIs on /agents", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("tiles reflect the seeded fleet + cost", async ({ page }) => {
    await insertAgent({ id: "alpha" });
    await insertAgent({ id: "beta" });
    await insertInbox("alpha", "hi");
    await insertInbox("alpha", "still there?");
    // Two turns, total 1500 + 2500 + 500 + 200 = 4700 cents = $47.00.
    await insertTurnCost({
      agentId: "alpha",
      model: "claude-sonnet-4-6",
      inputTokens: 1000,
      outputTokens: 500,
      inputUsdCents: 1500,
      outputUsdCents: 2500,
    });
    await insertTurnCost({
      agentId: "beta",
      model: "claude-opus-4-6",
      inputTokens: 200,
      outputTokens: 300,
      inputUsdCents: 500,
      outputUsdCents: 200,
    });

    await page.goto("/agents");
    const strip = page.getByTestId("kpi-strip");
    await expect(strip).toBeVisible();
    await expect(page.getByTestId("kpi-active")).toContainText("2/2");
    await expect(page.getByTestId("kpi-inbox")).toContainText("2");
    await expect(page.getByTestId("kpi-cost-today")).toContainText("$47");
    await expect(page.getByTestId("cost-donut")).toBeVisible();
  });

  test("system health card reports pool stats", async ({ page }) => {
    await insertAgent({ id: "solo" });
    await page.goto("/agents");
    const h = page.getByTestId("system-health-card");
    await expect(h).toBeVisible();
    await expect(page.getByTestId("health-pg")).toContainText("pg:");
    await expect(page.getByTestId("health-uptime")).toContainText("up ");
  });
});

test.describe("jobs page", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("renders scheduled jobs and webhook handlers", async ({ page }) => {
    await insertAgent({ id: "j" });
    await insertScheduledJob({ name: "nightly-sync", agentId: "j" });
    await insertScheduledJob({
      name: "weekly-audit",
      agentId: "j",
      enabled: false,
      cronExpr: "0 9 * * MON",
    });
    await insertWebhookHandler({ name: "gh-push", agentId: "j" });

    await page.goto("/jobs");
    await expect(page.getByTestId("jobs-page")).toBeVisible();
    await expect(page.getByTestId("scheduled-nightly-sync")).toContainText(
      "nightly-sync",
    );
    await expect(page.getByTestId("scheduled-weekly-sync")).not.toBeVisible({
      timeout: 100,
    });
    await expect(page.getByTestId("scheduled-weekly-audit")).toContainText(
      "disabled",
    );
    await expect(page.getByTestId("webhook-gh-push")).toContainText(
      "/hook/gh-push",
    );
  });

  test("empty state when no jobs", async ({ page }) => {
    await page.goto("/jobs");
    await expect(page.getByTestId("jobs-scheduled-empty")).toBeVisible();
    await expect(page.getByTestId("jobs-webhooks-empty")).toBeVisible();
  });
});
