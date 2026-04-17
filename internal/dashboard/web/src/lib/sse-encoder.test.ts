import { describe, expect, it } from "vitest";

import { encodeSSE } from "./sse-encoder";

describe("encodeSSE", () => {
  it("encodes a simple JSON data frame", () => {
    const out = encodeSSE({ event: "agent.status", data: { id: "m" } });
    expect(out).toContain("event: agent.status");
    expect(out).toContain('data: {"id":"m"}');
    expect(out.endsWith("\n\n")).toBe(true);
  });

  it("splits multi-line data per SSE spec", () => {
    const out = encodeSSE({ event: "x", data: "line1\nline2" });
    const lines = out.split("\n");
    expect(lines).toContain("data: line1");
    expect(lines).toContain("data: line2");
  });

  it("omits the event field when absent", () => {
    const out = encodeSSE({ data: { ok: true } });
    expect(out).not.toContain("event:");
    expect(out).toContain('data: {"ok":true}');
  });

  it("emits retry hint when set", () => {
    const out = encodeSSE({ data: {}, retry: 5000 });
    expect(out).toContain("retry: 5000");
  });

  it("preserves id for resumable streams", () => {
    const out = encodeSSE({ data: {}, id: "42" });
    expect(out).toContain("id: 42");
  });
});
