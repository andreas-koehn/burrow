import { describe, it, expect } from "vitest";
import { clientNameError } from "./clientName";

describe("clientNameError", () => {
  it.each(["a", "edge-01", "office-box-1", "x".repeat(63)])("accepts %s", (n) => {
    expect(clientNameError(n)).toBeNull();
  });
  it.each([
    ["", "Enter a name."],
    ["UI Audit Test!", "Use lowercase letters, digits and hyphens only."],
    ["Edge", "Use lowercase letters, digits and hyphens only."],
    ["a b", "Use lowercase letters, digits and hyphens only."],
    ["-edge", "Start and end with a letter or digit."],
    ["edge-", "Start and end with a letter or digit."],
    ["x".repeat(64), "Use at most 63 characters."],
  ])("rejects %j", (n, msg) => {
    expect(clientNameError(n)).toBe(msg);
  });
});
