import { describe, it, expect } from "vitest";
import { PROVIDER_PRESETS, envVarForSlot } from "./providerPresets";

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
    expect([...keys].sort()).toEqual(["baseUrl", "billing", "credentialSlot", "id", "label", "name", "note", "slug"]);
  });
});
