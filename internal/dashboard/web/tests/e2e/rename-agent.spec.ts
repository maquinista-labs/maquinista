import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  pgUrlFromState,
  withDb,
} from "./support/db";

// G.3 gate — operator can rename an agent from the detail page.
// Display flips to the new handle, id stays stable, and duplicates
// surface a targeted toast.

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

test.describe("rename agent", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("title renders id when no handle is set", async ({ page }) => {
    await insertAgent({ id: "t-9-1" });
    await page.goto("/agents/t-9-1");
    await expect(page.getByTestId("agent-detail-title")).toHaveText("t-9-1");
  });

  test("title renders the handle when set", async ({ page }) => {
    await insertAgent({ id: "t-9-2", handle: "already-named" });
    await page.goto("/agents/t-9-2");
    await expect(page.getByTestId("agent-detail-title")).toHaveText(
      "already-named",
    );
  });

  test("agent cards on /agents use the display name helper", async ({
    page,
  }) => {
    await insertAgent({ id: "t-9-3", handle: "my-coder" });
    await page.goto("/agents");
    await expect(
      page.getByTestId("agent-card-title").filter({ hasText: "my-coder" }),
    ).toBeVisible();
  });

  test("POST /api/agents/:id/rename writes the handle (API-level)", async ({
    request,
  }) => {
    await insertAgent({ id: "t-9-4" });
    const res = await request.post("/api/agents/t-9-4/rename", {
      data: { handle: "renamed-one" },
    });
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body.handle).toBe("renamed-one");

    const row = await withDb((c) =>
      c.query(`SELECT handle FROM agents WHERE id = $1`, ["t-9-4"]),
    );
    expect(row.rows[0].handle).toBe("renamed-one");
  });

  test("409 on handle collision, no mutation to the loser", async ({
    request,
  }) => {
    await insertAgent({ id: "first", handle: "taken" });
    await insertAgent({ id: "second" });

    const res = await request.post("/api/agents/second/rename", {
      data: { handle: "taken" },
    });
    expect(res.status()).toBe(409);
    const body = await res.json();
    expect(body.error).toBe("handle_taken");
    expect(body.handle).toBe("taken");

    const row = await withDb((c) =>
      c.query(`SELECT handle FROM agents WHERE id = $1`, ["second"]),
    );
    expect(row.rows[0].handle).toBeNull();
  });

  test("400 on invalid handle format", async ({ request }) => {
    await insertAgent({ id: "badfmt" });
    const bad = await request.post("/api/agents/badfmt/rename", {
      data: { handle: "Has SPACE" },
    });
    expect(bad.status()).toBe(400);

    // Reserved `t-` prefix is rejected by application layer.
    const reserved = await request.post("/api/agents/badfmt/rename", {
      data: { handle: "t-1-1" },
    });
    expect(reserved.status()).toBe(400);
  });

  test("404 on unknown id", async ({ request }) => {
    const res = await request.post("/api/agents/nope/rename", {
      data: { handle: "whoever" },
    });
    expect(res.status()).toBe(404);
  });

  test("clearing the handle (null body) reverts display to id", async ({
    request,
  }) => {
    await insertAgent({ id: "revertible", handle: "old-name" });
    const res = await request.post("/api/agents/revertible/rename", {
      data: { handle: null },
    });
    expect(res.status()).toBe(200);
    const row = await withDb((c) =>
      c.query(`SELECT handle FROM agents WHERE id = $1`, ["revertible"]),
    );
    expect(row.rows[0].handle).toBeNull();
  });
});
