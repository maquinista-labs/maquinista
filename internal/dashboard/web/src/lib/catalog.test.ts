import { describe, expect, it } from "vitest";

import {
  defaultModelFor,
  isKnownModel,
  isKnownRunner,
  MODELS,
  RUNNERS,
} from "./catalog";

describe("catalog", () => {
  it("every RUNNER has at least one MODEL entry", () => {
    for (const r of RUNNERS) {
      expect(MODELS[r.id]?.length ?? 0).toBeGreaterThan(0);
    }
  });

  it("defaultModelFor returns the first model in each runner's list", () => {
    expect(defaultModelFor("claude")).toBe("claude-opus-4-7");
    expect(defaultModelFor("openclaude")).toBe("GLM-5.1");
    expect(defaultModelFor("unknown")).toBeNull();
  });

  it("isKnownRunner / isKnownModel guard the picklist", () => {
    expect(isKnownRunner("claude")).toBe(true);
    expect(isKnownRunner("bogus")).toBe(false);
    expect(isKnownModel("claude", "claude-opus-4-7")).toBe(true);
    expect(isKnownModel("claude", "gpt-4o")).toBe(false);
    expect(isKnownModel("bogus", "any")).toBe(false);
  });
});
