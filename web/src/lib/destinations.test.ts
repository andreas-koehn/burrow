import { describe, it, expect } from "vitest";
import { destinationsFor, DESTINATIONS } from "./destinations";

describe("DESTINATIONS", () => {
  it("all paths are unique", () => {
    const paths = DESTINATIONS.map((d) => d.path);
    const unique = new Set(paths);
    expect(unique.size).toBe(paths.length);
  });

  it("all labels are unique", () => {
    const labels = DESTINATIONS.map((d) => d.label);
    const unique = new Set(labels);
    expect(unique.size).toBe(labels.length);
  });
});

describe("destinationsFor — admin with AI and http service", () => {
  const ctx = { isAdmin: true, hasAiEndpoints: true, firstHttpServiceId: "x" };
  const result = destinationsFor(ctx);
  const labels = result.map((d) => d.label);

  const expectedLabels = [
    "Home",
    "Clients",
    "Tunnels",
    "Services",
    "Tokens",
    "AI endpoints",
    "Cost & budgets",
    "Prompt cache",
    "Guardrails",
    "Request inspector",
    "Users",
    "Roles",
    "Settings",
    "Audit",
    "Webhooks",
    "Account",
    "Automation",
  ];

  for (const label of expectedLabels) {
    it(`includes "${label}"`, () => {
      expect(labels).toContain(label);
    });
  }

  it("Request inspector points at the stable /inspector entry route", () => {
    const inspector = result.find((d) => d.label === "Request inspector");
    expect(inspector?.path).toBe("/inspector");
  });
});

describe("destinationsFor — non-admin, no AI, no http service", () => {
  const ctx = { isAdmin: false, hasAiEndpoints: false };
  const result = destinationsFor(ctx);
  const labels = result.map((d) => d.label);

  const excludedAdminLabels = ["Users", "Roles", "Settings", "Audit", "Webhooks"];
  for (const label of excludedAdminLabels) {
    it(`excludes admin-only "${label}"`, () => {
      expect(labels).not.toContain(label);
    });
  }

  const excludedAiLabels = [
    "AI endpoints",
    "Cost & budgets",
    "Prompt cache",
    "Guardrails",
    "Request inspector",
  ];
  for (const label of excludedAiLabels) {
    it(`excludes AI group "${label}"`, () => {
      expect(labels).not.toContain(label);
    });
  }
});

describe("destinationsFor — non-admin with AI but no http service", () => {
  const ctx = { isAdmin: false, hasAiEndpoints: true, firstHttpServiceId: undefined };
  const result = destinationsFor(ctx);
  const labels = result.map((d) => d.label);

  it("excludes Request inspector when no firstHttpServiceId", () => {
    expect(labels).not.toContain("Request inspector");
  });

  it("includes other AI group items", () => {
    expect(labels).toContain("AI endpoints");
    expect(labels).toContain("Guardrails");
  });
});

describe("destinationsFor — non-admin with AI and http service", () => {
  const ctx = { isAdmin: false, hasAiEndpoints: true, firstHttpServiceId: "svc-42" };
  const result = destinationsFor(ctx);

  it("Request inspector points at the stable /inspector entry route", () => {
    const inspector = result.find((d) => d.label === "Request inspector");
    expect(inspector?.path).toBe("/inspector");
  });
});
