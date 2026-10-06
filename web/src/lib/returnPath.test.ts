import { describe, it, expect } from "vitest";
import { returnPathFrom } from "@/lib/returnPath";

describe("returnPathFrom", () => {
  it("takes a path of this dashboard, with its query and hash", () => {
    expect(returnPathFrom({ from: "/link?code=BRRW-7Q4K" })).toBe("/link?code=BRRW-7Q4K");
    expect(returnPathFrom({ from: "/clients?tab=tokens#x" })).toBe("/clients?tab=tokens#x");
  });

  it.each([
    ["no state", null],
    ["a state without from", {}],
    ["not a string", { from: 7 }],
    ["an absolute address", { from: "https://evil.example/link" }],
    ["a protocol-relative address", { from: "//evil.example/link" }],
    ["a backslash address", { from: "/\\evil.example" }],
    ["a backslash further in", { from: "/a\\b" }],
    ["a relative path", { from: "link?code=x" }],
    ["a script address", { from: "javascript:alert(1)" }],
    ["a control character", { from: "/link\n?code=x" }],
    ["the login page itself", { from: "/login" }],
    ["the login page with a query", { from: "/login?x=1" }],
    ["something very long", { from: "/" + "a".repeat(3000) }],
  ])("falls back to the start page for %s", (_name, state) => {
    expect(returnPathFrom(state)).toBe("/");
  });
});
