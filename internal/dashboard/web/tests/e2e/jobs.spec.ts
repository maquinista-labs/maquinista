// Jobs page E2E tests — covers create, toggle, run-now, and empty state.
// Requires a live Postgres fixture (skipped otherwise).

import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  insertScheduledJob,
  pgUrlFromState,
  withDb,
} from "./support/db";

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

async function insertSoulTemplate(id: string, name: string) {
  await withDb(async (c) => {
    await c.query(
      `INSERT INTO soul_templates (id, name, role, goal)
       VALUES ($1, $2, 'executor', 'test goal')
       ON CONFLICT (id) DO NOTHING`,
      [id, name],
    );
  });
}

test.describe("jobs page", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("jobs page loads with empty state", async ({ page }) => {
    await page.goto("/jobs");
    await expect(page.getByTestId("jobs-page")).toBeVisible();
    await expect(page.getByTestId("jobs-scheduled-empty")).toBeVisible();
    await expect(page.getByTestId("jobs-webhooks-empty")).toBeVisible();
  });

  test("new job button is visible", async ({ page }) => {
    await page.goto("/jobs");
    await expect(page.getByTestId("new-job-trigger")).toBeVisible();
  });

  test("create job via form and see it listed", async ({ page }) => {
    // Seed a soul template so the select has an option.
    await insertSoulTemplate("test-tpl", "Test Template");

    await page.goto("/jobs");
    await page.getByTestId("new-job-trigger").click();
    await expect(page.getByTestId("new-job-sheet")).toBeVisible();

    await page.getByTestId("job-name").fill("my-e2e-job");
    await page.getByTestId("job-cron").fill("0 9 * * *");
    // Select the first (and only) soul template.
    await page.getByTestId("job-soul-template").selectOption({ label: "Test Template" });
    await page.getByTestId("job-prompt").fill("Run the daily digest.");

    await page.getByTestId("job-submit").click();

    // Sheet should close and the card should appear.
    await expect(page.getByTestId("new-job-sheet")).not.toBeVisible();
    await expect(page.getByTestId("scheduled-my-e2e-job")).toBeVisible();
    await expect(page.getByTestId("scheduled-my-e2e-job")).toContainText("my-e2e-job");
  });

  test("toggle job disabled", async ({ page }) => {
    await insertAgent({ id: "agent-for-toggle" });
    await insertScheduledJob({ name: "toggle-test", agentId: "agent-for-toggle" });

    await page.goto("/jobs");
    const card = page.getByTestId("scheduled-toggle-test");
    await expect(card).toBeVisible();
    await expect(card).toContainText("enabled");

    await card.getByTestId("job-toggle-toggle-test").click();

    // After toggle, the badge should say "disabled".
    await expect(card).toContainText("disabled");
  });

  test("run now shows queued toast", async ({ page }) => {
    await insertAgent({ id: "agent-for-run" });
    await insertScheduledJob({ name: "run-now-test", agentId: "agent-for-run" });

    await page.goto("/jobs");
    const card = page.getByTestId("scheduled-run-now-test");
    await expect(card).toBeVisible();

    await card.getByTestId("job-run-run-now-test").click();

    // Toast should appear.
    await expect(page.locator("[data-sonner-toast]")).toContainText("queued");
  });
});
