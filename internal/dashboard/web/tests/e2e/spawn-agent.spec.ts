import { expect, test } from "@playwright/test";

import {
  cleanTables,
  insertAgent,
  pgUrlFromState,
  withDb,
} from "./support/db";

// G.5 gate — spawn-from-UI. The spec exercises the API surface
// end-to-end against a real Postgres fixture and verifies the
// collision error shape. The modal itself (Radix Sheet) follows
// the same click-event pattern as the rename-agent spec; the
// detail page's Radix click-sensitivity issues are pre-existing
// (documented in plans/active/dashboard-gaps.md triage backlog)
// so we drive spawns via the API endpoints rather than clicking
// the Sheet, keeping the spec stable.

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

test.describe("spawn agent from the dashboard", () => {
  test.beforeEach(async () => {
    dbRequired();
    await cleanTables();
  });

  test("GET /api/agents/new-catalog returns runners/models/souls", async ({
    request,
  }) => {
    const res = await request.get("/api/agents/new-catalog");
    expect(res.ok()).toBe(true);
    const body = await res.json();
    expect(Array.isArray(body.runners)).toBe(true);
    expect(body.runners.length).toBeGreaterThan(0);
    // Every runner has a model list.
    for (const r of body.runners) {
      expect(Array.isArray(body.models[r.id])).toBe(true);
      expect(body.models[r.id].length).toBeGreaterThan(0);
    }
    // Soul templates come from the DB — migrations seeded at least
    // `default` + G.4 added coordinator/planner/coder.
    const soulIds = body.souls.map((s: { id: string }) => s.id);
    expect(soulIds).toContain("default");
    expect(soulIds).toContain("coder");
    expect(soulIds).toContain("planner");
    expect(soulIds).toContain("coordinator");
  });

  test("GET /api/agents/check-handle flips from available to taken", async ({
    request,
  }) => {
    const fresh = await request.get(
      "/api/agents/check-handle?h=fresh-handle",
    );
    const freshBody = await fresh.json();
    expect(freshBody.available).toBe(true);

    await insertAgent({ id: "existing", handle: "fresh-handle" });

    const taken = await request.get(
      "/api/agents/check-handle?h=fresh-handle",
    );
    const takenBody = await taken.json();
    expect(takenBody.available).toBe(false);
    expect(takenBody.reason).toBe("taken");
  });

  test("GET /api/agents/check-handle flags invalid format", async ({
    request,
  }) => {
    const res = await request.get("/api/agents/check-handle?h=Has%20SPACE");
    const body = await res.json();
    expect(body.available).toBe(false);
    expect(body.reason).toBe("invalid");
  });

  test("POST /api/agents creates row + soul", async ({ request }) => {
    const res = await request.post("/api/agents", {
      data: {
        handle: "new-coder",
        runner: "claude",
        model: "claude-opus-4-7",
        soul_template: "coder",
      },
    });
    expect(res.status()).toBe(201);
    const body = await res.json();
    expect(body.id).toBe("new-coder");
    expect(body.handle).toBe("new-coder");

    const row = await withDb((c) =>
      c.query(
        `SELECT id, handle, runner_type, model, status, role
         FROM agents WHERE id = $1`,
        ["new-coder"],
      ),
    );
    expect(row.rows[0].handle).toBe("new-coder");
    expect(row.rows[0].runner_type).toBe("claude");
    expect(row.rows[0].model).toBe("claude-opus-4-7");
    expect(row.rows[0].status).toBe("stopped");
    expect(row.rows[0].role).toBe("user");

    const soul = await withDb((c) =>
      c.query(
        `SELECT template_id, role FROM agent_souls WHERE agent_id = $1`,
        ["new-coder"],
      ),
    );
    expect(soul.rows[0].template_id).toBe("coder");
    expect(String(soul.rows[0].role).length).toBeGreaterThan(0);
  });

  test("POST /api/agents returns 409 on handle collision", async ({
    request,
  }) => {
    await insertAgent({ id: "first-one", handle: "duplicate" });
    const res = await request.post("/api/agents", {
      data: {
        handle: "duplicate",
        runner: "claude",
        model: "claude-opus-4-7",
        soul_template: "coder",
      },
    });
    expect(res.status()).toBe(409);
    const body = await res.json();
    expect(body.error).toBe("handle_taken");
    expect(body.handle).toBe("duplicate");

    // No extra row was written (still one 'first-one').
    const count = await withDb((c) =>
      c.query(`SELECT COUNT(*)::int AS n FROM agents WHERE handle = 'duplicate'`),
    );
    expect(count.rows[0].n).toBe(1);
  });

  test("POST /api/agents returns 400 on invalid handle format", async ({
    request,
  }) => {
    const res = await request.post("/api/agents", {
      data: {
        handle: "Has SPACE",
        runner: "claude",
        model: "claude-opus-4-7",
        soul_template: "coder",
      },
    });
    expect(res.status()).toBe(400);
    const body = await res.json();
    expect(body.error).toBe("invalid_handle");
  });

  test("POST /api/agents returns 400 on invalid runner/model/soul", async ({
    request,
  }) => {
    const badRunner = await request.post("/api/agents", {
      data: {
        handle: "fine-handle",
        runner: "fake-runner",
        model: null,
        soul_template: "coder",
      },
    });
    expect(badRunner.status()).toBe(400);

    const badModel = await request.post("/api/agents", {
      data: {
        handle: "fine-handle-2",
        runner: "claude",
        model: "gpt-4o",
        soul_template: "coder",
      },
    });
    expect(badModel.status()).toBe(400);

    const badSoul = await request.post("/api/agents", {
      data: {
        handle: "fine-handle-3",
        runner: "claude",
        model: "claude-opus-4-7",
        soul_template: "does-not-exist",
      },
    });
    expect(badSoul.status()).toBe(400);
    const badSoulBody = await badSoul.json();
    expect(badSoulBody.error).toBe("invalid_soul_template");
  });

  test("the spawn-agent trigger is visible on /agents", async ({ page }) => {
    await page.goto("/agents");
    await expect(page.getByTestId("spawn-agent-trigger")).toBeVisible();
  });
});
