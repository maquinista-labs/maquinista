import { describe, expect, it } from "vitest";

import {
  hashPassword,
  hashToken,
  isLocked,
  newSessionToken,
  verifyPassword,
} from "./auth";

describe("hashPassword / verifyPassword", () => {
  it("round-trips", () => {
    const { hash, salt, iter } = hashPassword("s3cret!", undefined, 1000);
    expect(verifyPassword("s3cret!", hash, salt, iter)).toBe(true);
  });

  it("rejects wrong password", () => {
    const { hash, salt, iter } = hashPassword("right", undefined, 1000);
    expect(verifyPassword("wrong", hash, salt, iter)).toBe(false);
  });

  it("produces different hashes for different salts", () => {
    const a = hashPassword("x", "aaaa", 1000);
    const b = hashPassword("x", "bbbb", 1000);
    expect(a.hash).not.toBe(b.hash);
  });
});

describe("newSessionToken", () => {
  it("returns different tokens each call", () => {
    const a = newSessionToken();
    const b = newSessionToken();
    expect(a.token).not.toBe(b.token);
    expect(a.hash).not.toBe(b.hash);
  });

  it("hashToken is deterministic", () => {
    expect(hashToken("abc")).toBe(hashToken("abc"));
  });
});

describe("isLocked", () => {
  it("returns false for null", () => {
    expect(isLocked(null)).toBe(false);
  });

  it("returns true for a future date", () => {
    expect(isLocked(new Date(Date.now() + 60_000))).toBe(true);
  });

  it("returns false for a past date", () => {
    expect(isLocked(new Date(Date.now() - 60_000))).toBe(false);
  });
});
