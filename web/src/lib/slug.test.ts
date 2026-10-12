import { describe, it, expect } from "vitest";
import { deriveServiceId, deriveSlug, serviceIdError } from "./slug";
import { SLUG_RE } from "@/components/SlugField";

describe("deriveServiceId", () => {
  it("lowercases and turns every run of other characters into one hyphen", () => {
    expect(deriveServiceId("Web Prod")).toBe("web-prod");
    expect(deriveServiceId("  Grafana (EU) / 2  ")).toBe("grafana-eu-2");
  });

  it("keeps what a service id may hold: letters, digits, hyphens and underscores", () => {
    expect(deriveServiceId("ci_runner-2")).toBe("ci_runner-2");
  });

  it("begins and ends with a letter or a digit, so a command line can take it", () => {
    expect(deriveServiceId("--web--")).toBe("web");
    expect(deriveServiceId("_internal_")).toBe("internal");
  });

  it("is cut to 64 characters, without a hyphen left at the end", () => {
    expect(deriveServiceId("a".repeat(80))).toBe("a".repeat(64));
    expect(deriveServiceId(`${"a".repeat(63)} b`)).toBe("a".repeat(63));
  });

  it("gives nothing for a name without a letter or a digit", () => {
    expect(deriveServiceId("!!!")).toBe("");
    expect(deriveServiceId("")).toBe("");
  });
});

describe("deriveSlug", () => {
  it("follows the relay's slug rule", () => {
    for (const name of ["Web Prod", "ci_runner-2", "  Grafana (EU) / 2  ", "x".repeat(80), `${"a".repeat(39)} b`]) {
      expect(deriveSlug(name)).toMatch(SLUG_RE);
    }
    expect(deriveSlug("Web Prod")).toBe("web-prod");
    // A slug has no underscore.
    expect(deriveSlug("ci_runner-2")).toBe("ci-runner-2");
  });

  it("is cut to 40 characters, without a hyphen left at the end", () => {
    expect(deriveSlug("x".repeat(80))).toBe("x".repeat(40));
    expect(deriveSlug(`${"a".repeat(39)} b`)).toBe("a".repeat(39));
  });

  it("gives nothing for a name without a letter or a digit", () => {
    expect(deriveSlug("!!!")).toBe("");
  });
});

describe("serviceIdError", () => {
  it("accepts what the relay accepts", () => {
    for (const id of ["web", "web-prod", "ci_runner-2", "a".repeat(64)]) expect(serviceIdError(id)).toBeNull();
  });

  it("names the rule for anything else", () => {
    for (const id of ["ab", "Web", "web prod", "a".repeat(65)]) expect(serviceIdError(id)).toMatch(/3.64 characters/);
  });
});
