import { describe, expect, it } from "vitest";

import { displayName, HANDLE_REGEX, isValidHandle } from "./utils";

describe("isValidHandle", () => {
  it("accepts 2-32 char lowercase alphanumerics + hyphen + underscore", () => {
    expect(isValidHandle("ok")).toBe(true);
    expect(isValidHandle("coder")).toBe(true);
    expect(isValidHandle("a_b-c")).toBe(true);
    expect(isValidHandle("a".repeat(32))).toBe(true);
  });

  it("rejects empty, too-short, too-long, or wrong-case", () => {
    expect(isValidHandle("")).toBe(false);
    expect(isValidHandle("a")).toBe(false);
    expect(isValidHandle("a".repeat(33))).toBe(false);
    expect(isValidHandle("Coder")).toBe(false);
    expect(isValidHandle("with space")).toBe(false);
    expect(isValidHandle("has.dot")).toBe(false);
  });

  it("forbids the reserved `t-` prefix (shadows auto-ids)", () => {
    expect(isValidHandle("t-1-1")).toBe(false);
    expect(isValidHandle("t-anything")).toBe(false);
    expect(HANDLE_REGEX.test("t-1-1")).toBe(true); // regex alone allows it
  });
});

describe("displayName", () => {
  it("returns handle when set, else id", () => {
    expect(displayName({ handle: "coder", id: "t-1-1" })).toBe("coder");
    expect(displayName({ handle: null, id: "t-1-1" })).toBe("t-1-1");
  });
});
