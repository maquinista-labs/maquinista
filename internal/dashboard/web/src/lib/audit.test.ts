import { afterEach, describe, expect, it } from "vitest";

import { _resetRateLimiter, rateLimit, extractClientIp } from "./audit";

afterEach(() => _resetRateLimiter());

describe("rateLimit", () => {
  it("allows under the cap", () => {
    for (let i = 0; i < 5; i++) {
      expect(rateLimit("k", 10, 0).allowed).toBe(true);
    }
  });

  it("blocks past the cap", () => {
    for (let i = 0; i < 10; i++) rateLimit("k", 10, 0);
    expect(rateLimit("k", 10, 0).allowed).toBe(false);
  });

  it("resets the window after a minute", () => {
    for (let i = 0; i < 10; i++) rateLimit("k", 10, 0);
    expect(rateLimit("k", 10, 0).allowed).toBe(false);
    expect(rateLimit("k", 10, 61_000).allowed).toBe(true);
  });

  it("keys independently", () => {
    for (let i = 0; i < 10; i++) rateLimit("a", 10, 0);
    expect(rateLimit("b", 10, 0).allowed).toBe(true);
  });
});

describe("extractClientIp", () => {
  it("reads x-forwarded-for first", () => {
    const req = new Request("http://x/", {
      headers: { "x-forwarded-for": "1.2.3.4, 5.6.7.8" },
    });
    expect(extractClientIp(req)).toBe("1.2.3.4");
  });

  it("falls back to x-real-ip", () => {
    const req = new Request("http://x/", {
      headers: { "x-real-ip": "9.9.9.9" },
    });
    expect(extractClientIp(req)).toBe("9.9.9.9");
  });

  it("returns null with no hints", () => {
    expect(extractClientIp(new Request("http://x/"))).toBeNull();
  });
});
