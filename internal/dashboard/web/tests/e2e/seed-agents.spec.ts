import { expect, test } from "@playwright/test";

import {
  cleanTables,
  pgUrlFromState,
  withDb,
} from "./support/db";

// G.4 gate — once the orchestrator's startup seed helper has run,
// /agents shows the coordinator/planner/coder trio with their
// handles as display names. Playwright doesn't spin up the
// orchestrator (that path lives in a Go integration test); here we
// simulate the post-seed DB state and assert the dashboard renders
// the expected shape.

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

async function seedThreeAgents() {
  await withDb(async (c) => {
    for (const [id, handle] of [
      ["seed-coordinator", "coordinator"],
      ["seed-planner", "planner"],
      ["seed-coder", "coder"],
    ]) {
      await c.query(
        `INSERT INTO agents
           (id, handle, tmux_session, tmux_window, role, status,
            runner_type, cwd, window_name, started_at, last_seen,
            stop_requested)
         VALUES ($1, $2, 'maquinista', '', 'user', 'stopped',
                 'claude', '/tmp', $1, NOW(), NOW(), FALSE)`,
        [id, handle],
      );
    }
  });
}

test.describe("default seeded agents render on /agents", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("all three archetype handles show as display names", async ({
    page,
  }) => {
    await seedThreeAgents();
    await page.goto("/agents");

    const coordinator = page.getByTestId("agent-card-title").filter({
      hasText: /^coordinator$/,
    });
    const planner = page.getByTestId("agent-card-title").filter({
      hasText: /^planner$/,
    });
    const coder = page.getByTestId("agent-card-title").filter({
      hasText: /^coder$/,
    });
    await expect(coordinator).toBeVisible();
    await expect(planner).toBeVisible();
    await expect(coder).toBeVisible();
  });

  test("detail title shows handle, id stays in the URL", async ({ page }) => {
    await seedThreeAgents();
    await page.goto("/agents/seed-coder");
    await expect(page.getByTestId("agent-detail-title")).toHaveText("coder");
    await expect(page).toHaveURL(/\/agents\/seed-coder$/);
  });
});
