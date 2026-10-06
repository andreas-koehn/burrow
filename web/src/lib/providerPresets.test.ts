import { describe, it, expect } from "vitest";
import { PROVIDER_PRESETS, credentialSlotError, envVarForSlot } from "./providerPresets";

describe("provider presets", () => {
  it("ships the three known providers and a blank custom entry", () => {
    const byId = Object.fromEntries(PROVIDER_PRESETS.map((p) => [p.id, p]));
    expect(byId["openrouter"]).toMatchObject({
      slug: "openrouter", baseUrl: "https://openrouter.ai/api/v1", credentialSlot: "OPENROUTER", billing: "metered",
    });
    expect(byId["zai-coding"]).toMatchObject({
      slug: "zai", baseUrl: "https://api.z.ai/api/coding/paas/v4", credentialSlot: "ZAI", billing: "flat",
    });
    expect(byId["zai-api"]).toMatchObject({
      slug: "zai", baseUrl: "https://api.z.ai/api/paas/v4", credentialSlot: "ZAI", billing: "metered",
    });
    expect(byId["custom"]).toMatchObject({ slug: "", baseUrl: "", credentialSlot: "" });
  });
  it("every preset URL is https and has no trailing slash", () => {
    for (const p of PROVIDER_PRESETS.filter((p) => p.baseUrl)) {
      expect(p.baseUrl).toMatch(/^https:\/\/[^/]+\/.*[^/]$/);
    }
  });
  it("names the environment variable for a slot", () => {
    expect(envVarForSlot("OPENROUTER")).toBe("BURROW_UPSTREAM_KEY_OPENROUTER");
  });
  it("carries nothing but public endpoint data", () => {
    const keys = new Set(PROVIDER_PRESETS.flatMap((p) => Object.keys(p)));
    expect([...keys].sort()).toEqual(["baseUrl", "billing", "credentialSlot", "id", "label", "name", "note", "slug", "supportsResponses"]);
  });
  it("marks only OpenRouter as offering the Responses API", () => {
    expect(Object.fromEntries(PROVIDER_PRESETS.map((p) => [p.id, p.supportsResponses]))).toEqual({
      openrouter: true, "zai-coding": false, "zai-api": false, custom: false,
    });
  });
  it("accepts a slot name the relay accepts and rejects the rest", () => {
    expect(credentialSlotError("")).toBeNull(); // nothing typed yet
    expect(credentialSlotError("OPENROUTER")).toBeNull();
    expect(credentialSlotError("TEAM_A_2")).toBeNull();
    expect(credentialSlotError("open-router")).toMatch(/A–Z, 0–9 and underscore/);
    expect(credentialSlotError("A".repeat(33))).toMatch(/1–32 characters/);
  });
  it("rejects a slot ending in _FILE: the relay reads that variable as a file path for another slot", () => {
    expect(credentialSlotError("FOO_FILE")).toMatch(/cannot end in _FILE/);
    expect(credentialSlotError("FOO_FILE")).toContain("BURROW_UPSTREAM_KEY_FOO_FILE");
    expect(credentialSlotError("_FILE")).toMatch(/cannot end in _FILE/);
    expect(credentialSlotError("FILE")).toBeNull();
    expect(credentialSlotError("FILE_A")).toBeNull();
  });
  for (const p of PROVIDER_PRESETS.filter((p) => p.credentialSlot)) {
    it(`preset ${p.id} names a valid slot`, () => expect(credentialSlotError(p.credentialSlot)).toBeNull());
  }
});
