import { expect, test } from "@playwright/test";

/**
 * Shell smoke tests: header, bottom nav (3 tabs), routing, theme toggle.
 */

test.describe("dashboard shell", () => {
  test("root redirects to /agents", async ({ page }) => {
    await page.goto("/");
    await expect(page).toHaveURL(/\/agents$/);
  });

  test("sticky header renders with app title", async ({ page }) => {
    await page.goto("/agents");
    const header = page.getByTestId("dash-header");
    await expect(header).toBeVisible();
    await expect(header).toContainText("maquinista");
  });

  test("bottom nav has three tabs and active state", async ({ page }) => {
    await page.goto("/agents");
    const nav = page.getByTestId("bottom-nav");
    await expect(nav).toBeVisible();

    await expect(page.getByTestId("nav-agents")).toBeVisible();
    await expect(page.getByTestId("nav-inbox")).toBeVisible();
    await expect(page.getByTestId("nav-jobs")).toBeVisible();

    // The agents tab is marked current.
    await expect(page.getByTestId("nav-agents")).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  test("navigating to a tab switches the active indicator", async ({
    page,
  }) => {
    await page.goto("/agents");
    await page.getByTestId("nav-inbox").click();
    await expect(page).toHaveURL(/\/inbox$/);
    await expect(page.getByTestId("nav-inbox")).toHaveAttribute(
      "aria-current",
      "page",
    );
    await expect(page.getByTestId("nav-agents")).not.toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  test("every top-level route renders its heading", async ({ page }) => {
    for (const [href, title] of [
      ["/agents", "Agents"],
      ["/inbox", "Inbox"],
      ["/jobs", "Jobs"],
    ] as const) {
      await page.goto(href);
      await expect(
        page.getByRole("heading", { name: title }),
      ).toBeVisible();
    }
  });

  test("theme toggle cycles system → light → dark", async ({ page }) => {
    await page.goto("/agents");
    const toggle = page.getByTestId("theme-toggle");
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute("data-theme-mode", "system");
    await expect(toggle).toHaveAttribute("data-theme-next", "light");

    await toggle.click();
    await expect(toggle).toHaveAttribute("data-theme-mode", "light");
    await expect(toggle).toHaveAttribute("data-theme-next", "dark");

    await toggle.click();
    await expect(toggle).toHaveAttribute("data-theme-mode", "dark");
    await expect(toggle).toHaveAttribute("data-theme-next", "system");
  });

  test("/api/healthz returns ok:true from the real Next handler", async ({
    request,
  }) => {
    const res = await request.get("/api/healthz");
    expect(res.ok()).toBe(true);
    const body = await res.json();
    expect(body.ok).toBe(true);
    expect(body.stub).toBe(false);
    expect(typeof body.pid).toBe("number");
  });
});
