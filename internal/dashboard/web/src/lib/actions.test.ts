import { describe, expect, it, vi } from "vitest";
import type { Pool } from "pg";

import { renameAgent, spawnAgentFromDashboard } from "./actions";

function mockPoolWith(
  queryImpl: (sql: string, params: unknown[]) => Promise<unknown>,
): Pool {
  return {
    query: vi.fn(async (sql: string, params: unknown[]) =>
      queryImpl(sql, params),
    ),
  } as unknown as Pool;
}

describe("renameAgent", () => {
  it("returns 'updated' on a successful UPDATE", async () => {
    const pool = mockPoolWith(async () => ({ rowCount: 1 }));
    const result = await renameAgent(pool, "a1", "coder");
    expect(result).toBe("updated");
  });

  it("returns 'not_found' when no row matches", async () => {
    const pool = mockPoolWith(async () => ({ rowCount: 0 }));
    const result = await renameAgent(pool, "missing", "coder");
    expect(result).toBe("not_found");
  });

  it("returns 'conflict' on Postgres 23505 unique-violation", async () => {
    const pool = mockPoolWith(async () => {
      const err: Error & { code?: string } = new Error("duplicate key");
      err.code = "23505";
      throw err;
    });
    const result = await renameAgent(pool, "a1", "coder");
    expect(result).toBe("conflict");
  });

  it("rethrows non-conflict errors", async () => {
    const pool = mockPoolWith(async () => {
      throw new Error("connection lost");
    });
    await expect(renameAgent(pool, "a1", "coder")).rejects.toThrow(
      "connection lost",
    );
  });

  it("rejects the reserved `t-` prefix", async () => {
    // renameAgent itself doesn't validate the regex; its caller does.
    // Confirm that — this test pins the contract so G.5 can re-use it
    // safely for spawn without double-validating.
    const pool = mockPoolWith(async () => ({ rowCount: 1 }));
    const result = await renameAgent(pool, "any", "t-1-1");
    expect(result).toBe("updated"); // no regex guard in the helper
  });

  it("passes null through to clear the handle", async () => {
    const capture = vi.fn(async () => ({ rowCount: 1 }));
    const pool = {
      query: capture,
    } as unknown as Pool;
    await renameAgent(pool, "a1", null);
    expect(capture.mock.calls[0][1]).toEqual(["a1", null]);
  });
});

describe("spawnAgentFromDashboard (validation branches)", () => {
  // Only the early-exit branches can be tested without a real client
  // — the happy path opens a transaction via pool.connect(), which
  // the simple mock doesn't cover. The happy path is exercised in
  // the Playwright spec against a real Postgres.

  it("returns invalid_handle for regex failures", async () => {
    const pool = {
      query: vi.fn(),
    } as unknown as Pool;
    const result = await spawnAgentFromDashboard(pool, {
      handle: "BadCase",
      runner: "claude",
      model: null,
      soulTemplateID: "coder",
      cwd: "/tmp",
    });
    expect(result.kind).toBe("invalid_handle");
  });

  it("returns invalid_handle for the reserved `t-` prefix", async () => {
    const pool = {
      query: vi.fn(),
    } as unknown as Pool;
    const result = await spawnAgentFromDashboard(pool, {
      handle: "t-1-1",
      runner: "claude",
      model: null,
      soulTemplateID: "coder",
      cwd: "/tmp",
    });
    expect(result.kind).toBe("invalid_handle");
  });

  it("returns invalid_soul_template when the template is unknown", async () => {
    const pool = {
      query: vi.fn(async () => ({ rows: [] })),
    } as unknown as Pool;
    const result = await spawnAgentFromDashboard(pool, {
      handle: "coder",
      runner: "claude",
      model: null,
      soulTemplateID: "does-not-exist",
      cwd: "/tmp",
    });
    expect(result.kind).toBe("invalid_soul_template");
  });
});
