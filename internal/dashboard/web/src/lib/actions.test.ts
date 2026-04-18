import { describe, expect, it, vi } from "vitest";
import type { Pool } from "pg";

import { renameAgent } from "./actions";

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

  it("passes null through to clear the handle", async () => {
    const capture = vi.fn(async () => ({ rowCount: 1 }));
    const pool = {
      query: capture,
    } as unknown as Pool;
    await renameAgent(pool, "a1", null);
    expect(capture.mock.calls[0][1]).toEqual(["a1", null]);
  });
});
