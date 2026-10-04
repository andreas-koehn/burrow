import { describe, it, expect } from "vitest";
import { shellQuote } from "./shell";

describe("shellQuote", () => {
  it.each(["edge-01", "127.0.0.1:3000", "burrow.example.com:7000", "bur_abc123", "a/b_c.d"])(
    "leaves safe arg %s untouched", (a) => { expect(shellQuote(a)).toBe(a); });
  it("quotes spaces and metacharacters", () => {
    expect(shellQuote("UI Audit Test!")).toBe("'UI Audit Test!'");
    expect(shellQuote("a;rm -rf /")).toBe("'a;rm -rf /'");
    expect(shellQuote("$(whoami)")).toBe("'$(whoami)'");
  });
  it("escapes embedded single quotes", () => {
    expect(shellQuote("it's")).toBe("'it'\\''s'");
  });
  it("quotes the empty string", () => {
    expect(shellQuote("")).toBe("''");
  });
});
