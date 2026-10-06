import { describe, it, expect } from "vitest";
import { modelNameError } from "./modelNames";

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
