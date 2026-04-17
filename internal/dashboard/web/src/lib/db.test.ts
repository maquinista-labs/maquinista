import { afterEach, describe, expect, it, vi } from "vitest";

// Unit tests for the pg Pool singleton. We stub the `pg` module so
// these don't open real connections; the pg+Pg container coverage
// lives in the API route tests (Commit 2.2 onwards).

const makePool = () => {
  return {
    on: vi.fn(),
    end: vi.fn().mockResolvedValue(undefined),
    query: vi.fn(),
  };
};

vi.mock("pg", () => {
  const Pool = vi.fn(function (this: any) {
    Object.assign(this, makePool());
  });
  return { Pool };
});

afterEach(async () => {
  const db = await import("./db");
  db.setPoolOverride(undefined);
  await db.closePool();
  vi.clearAllMocks();
});

describe("getPool", () => {
  it("throws when DATABASE_URL is unset", async () => {
    delete process.env.DATABASE_URL;
    vi.resetModules();
    const { getPool } = await import("./db");
    expect(() => getPool()).toThrow(/DATABASE_URL/);
  });

  it("returns the same instance on repeated calls", async () => {
    process.env.DATABASE_URL = "postgres://test/db";
    vi.resetModules();
    const { getPool } = await import("./db");
    const a = getPool();
    const b = getPool();
    expect(a).toBe(b);
  });

  it("honours setPoolOverride for tests", async () => {
    const { setPoolOverride, getPool } = await import("./db");
    const fake = makePool() as unknown as ReturnType<typeof makePool>;
    setPoolOverride(fake as unknown as import("pg").Pool);
    expect(getPool()).toBe(fake);
    setPoolOverride(undefined);
  });

  it("closePool() allows a fresh pool after", async () => {
    process.env.DATABASE_URL = "postgres://test/db";
    vi.resetModules();
    const { getPool, closePool } = await import("./db");
    const first = getPool();
    await closePool();
    const second = getPool();
    expect(second).not.toBe(first);
  });
});
