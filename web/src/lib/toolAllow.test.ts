import { describe, it, expect } from "vitest";
import { toolEntryProblem } from "./toolAllow";

describe("toolEntryProblem", () => {
  it.each(["github__list_issues", "github__*", "a__b__c", "my-src9__x.y"])("accepts %s", (e) => {
    expect(toolEntryProblem(e)).toBe("");
  });
  it.each([
    ["src__read_*", "a * inside a tool name"],
    ["read_file", "no source"],
    ["github__", "no tool"],
    ["GitHub__x", "upper case source"],
    ["burrow__x", "reserved slug"],
    ["v1__x", "reserved slug"],
    ["a_b__x", "underscore in the source"],
    ["src__a b", "space"],
    ["src__a\tb", "control character"],
    ["src__a\u007fb", "DEL"],
    ["a".repeat(25) + "__x", "source over 24 characters"],
  ])("refuses %s (%s)", (e) => {
    expect(toolEntryProblem(e)).not.toBe("");
  });
  it("counts the length in bytes", () => {
    // 80 two-byte characters are 160 bytes plus the prefix: over the limit, though only 85 characters.
    expect(toolEntryProblem("src__" + "é".repeat(80))).not.toBe("");
    expect(toolEntryProblem("src__" + "é".repeat(77))).toBe("");
  });
});
