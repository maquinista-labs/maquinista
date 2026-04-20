import { expect, test } from "@playwright/test";

// Auth mode = "none" (the default): every route is accessible without
// any session cookie. These tests confirm the middleware passes
// through unauthenticated requests and the /auth page is reachable
// but not required.

test.describe("auth mode: none (default)", () => {
  test("dashboard routes are accessible without a session cookie", async ({
    page,
  }) => {
    // No cookie is set; all nav destinations should render, not
    // redirect to /auth.
    for (const href of ["/agents", "/inbox", "/jobs"]) {
      const res = await page.goto(href);
      expect(res?.status()).not.toBe(401);
      expect(res?.status()).not.toBe(403);
      await expect(page).not.toHaveURL(/\/auth/);
    }
  });

  test("API routes return data without a session cookie", async ({
    request,
  }) => {
    for (const path of ["/api/healthz", "/api/agents", "/api/kpis"]) {
      const res = await request.get(path);
      expect(res.status()).not.toBe(401);
      expect(res.status()).not.toBe(403);
    }
  });

  test("/auth page is reachable but not enforced", async ({ page }) => {
    // /auth is available (for deployments that add password later)
    // but navigating to a protected route does NOT redirect here.
    const res = await page.goto("/auth");
    expect(res?.status()).toBe(200);

    // Going to /agents stays on /agents — no forced redirect.
    await page.goto("/agents");
    await expect(page).toHaveURL(/\/agents/);
    await expect(page).not.toHaveURL(/\/auth/);
  });

  test("/api/healthz is always reachable", async ({ request }) => {
    const res = await request.get("/api/healthz");
    expect(res.ok()).toBe(true);
    const body = await res.json();
    expect(body.ok).toBe(true);
  });
});
