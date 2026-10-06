import { describe, it, expect } from "vitest";
import { modelNameError, allowEntryError } from "./modelNames";

describe("modelNameError", () => {
  it("accepts valid names, including names a client asks for by default", () => {
    for (const n of ["burrow-intelligence", "fast", "gpt4.mini_v2", "a1", "claude-sonnet-4-6"]) expect(modelNameError(n)).toBeNull();
  });
  it("rejects invalid names", () => {
    for (const n of ["A", "a", "a/b", "Burrow", "-x", "v1", "a b", "a".repeat(64)]) {
      expect(modelNameError(n)).not.toBeNull();
    }
  });
  it("treats empty as not chosen", () => expect(modelNameError("")).toBeNull());
});

describe("allowEntryError", () => {
  it("accepts model names and provider patterns", () => {
    for (const e of ["burrow-simple", "zai/*", "zai/glm-5.1", "openrouter/google/gemini-x"]) {
      expect(allowEntryError(e)).toBeNull();
    }
  });
  it("rejects malformed entries", () => {
    for (const e of ["*", "*/x", "zai/", "/x", "Zai/*", "zai/**", "a b", ""]) expect(allowEntryError(e)).not.toBeNull();
  });
});
