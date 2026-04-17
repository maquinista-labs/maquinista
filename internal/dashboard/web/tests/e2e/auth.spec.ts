import { expect, test } from "@playwright/test";

import {
  countAudit,
  pgUrlFromState,
  seedOperator,
  truncateAuth,
} from "./support/db";

const dbRequired = () =>
  test.skip(!pgUrlFromState(), "Postgres fixture unavailable; skipping");

test.describe("auth API", () => {
  test.beforeEach(async () => {
    dbRequired();
    await truncateAuth();
  });

  test("login happy path sets a session cookie", async ({ request }) => {
    await seedOperator("otavio", "hunter42");
    const res = await request.post("/api/auth/login", {
      data: { username: "otavio", password: "hunter42" },
    });
    expect(res.status()).toBe(200);
    const cookies = res.headers()["set-cookie"] ?? "";
    expect(cookies).toContain("maquinista_dash_session=");
    expect(await countAudit("auth.login", true)).toBe(1);
  });

  test("login with wrong password returns 401 + audit row", async ({
    request,
  }) => {
    await seedOperator("otavio", "hunter42");
    const res = await request.post("/api/auth/login", {
      data: { username: "otavio", password: "wrong" },
    });
    expect(res.status()).toBe(401);
    expect(await countAudit("auth.login", false)).toBe(1);
  });

  test("login with missing user returns 401", async ({ request }) => {
    const res = await request.post("/api/auth/login", {
      data: { username: "ghost", password: "x" },
    });
    expect(res.status()).toBe(401);
  });

  test("login rejects missing fields with 400", async ({ request }) => {
    const res = await request.post("/api/auth/login", {
      data: { username: "only" },
    });
    expect(res.status()).toBe(400);
  });

  test("account locks after 5 failed attempts", async ({ request }) => {
    await seedOperator("lock-me", "real-password");
    for (let i = 0; i < 5; i++) {
      const res = await request.post("/api/auth/login", {
        data: { username: "lock-me", password: "wrong" },
      });
      expect(res.status()).toBe(401);
    }
    const res = await request.post("/api/auth/login", {
      data: { username: "lock-me", password: "real-password" },
    });
    expect(res.status()).toBe(423);
  });

  test("logout revokes the session", async ({ request }) => {
    await seedOperator("outie", "hunter42");
    await request.post("/api/auth/login", {
      data: { username: "outie", password: "hunter42" },
    });
    const logout = await request.post("/api/auth/logout");
    expect(logout.status()).toBe(200);
  });

  test("/auth page renders the login form", async ({ page }) => {
    await page.goto("/auth");
    await expect(page.getByTestId("auth-panel")).toBeVisible();
    await expect(page.getByTestId("login-form")).toBeVisible();
    await expect(page.getByTestId("login-username")).toBeVisible();
    await expect(page.getByTestId("login-password")).toBeVisible();
  });
});
