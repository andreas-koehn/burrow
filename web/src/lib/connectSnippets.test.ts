import { describe, it, expect } from "vitest";
import { claudeCodeSettings, codexConfig, openAiSdkPython, openAiSdkTypeScript, genericProvider, pickTier } from "./connectSnippets";

const ANTHROPIC = "https://b.example.com/anthropic";
const OPENAI = "https://b.example.com/openai/v1";

describe("claudeCodeSettings", () => {
  it("is a settings.json env block with the base URL, a key placeholder and three pinned models", () => {
    const text = claudeCodeSettings(ANTHROPIC, { opus: "burrow-intelligence", sonnet: "burrow-medium", haiku: "burrow-simple" });
    expect(JSON.parse(text)).toEqual({
      env: {
        ANTHROPIC_BASE_URL: ANTHROPIC,
        ANTHROPIC_AUTH_TOKEN: "<your gateway key>",
        ANTHROPIC_DEFAULT_OPUS_MODEL: "burrow-intelligence",
        ANTHROPIC_DEFAULT_SONNET_MODEL: "burrow-medium",
        ANTHROPIC_DEFAULT_HAIKU_MODEL: "burrow-simple",
      },
    });
  });
  it("turns thinking off in Claude Code settings when a pinned model is translated", () => {
    const env = JSON.parse(claudeCodeSettings(ANTHROPIC, { sonnet: "burrow-simple" }, { thinkingOff: true })).env;
    expect(env.MAX_THINKING_TOKENS).toBe("0");
  });
  it("leaves thinking alone otherwise", () => {
    const env = JSON.parse(claudeCodeSettings(ANTHROPIC, { sonnet: "burrow-simple" })).env;
    expect("MAX_THINKING_TOKENS" in env).toBe(false);
  });
  it("leaves out a tier that has no model", () => {
    const env = JSON.parse(claudeCodeSettings(ANTHROPIC, { sonnet: "burrow-medium" })).env;
    expect(Object.keys(env)).toEqual(["ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_DEFAULT_SONNET_MODEL"]);
  });
});

describe("pickTier", () => {
  it("spreads the models over the three tiers in list order, repeating the last one", () => {
    expect(pickTier(["a", "b", "c", "d"])).toEqual({ opus: "a", sonnet: "b", haiku: "c" });
    expect(pickTier(["a", "b"])).toEqual({ opus: "a", sonnet: "b", haiku: "b" });
    expect(pickTier(["a"])).toEqual({ opus: "a", sonnet: "a", haiku: "a" });
    expect(pickTier([])).toEqual({});
  });
});

describe("codexConfig", () => {
  it("is a config.toml provider block that uses the Responses API", () => {
    expect(codexConfig(OPENAI, "burrow-medium")).toBe(
      [
        'model = "burrow-medium"',
        'model_provider = "burrow"',
        "",
        "[model_providers.burrow]",
        'name = "Burrow"',
        `base_url = "${OPENAI}"`,
        'env_key = "BURROW_GATEWAY_KEY"',
        'wire_api = "responses"',
      ].join("\n"),
    );
  });
  it("escapes a model name for TOML", () => {
    expect(codexConfig(OPENAI, 'we"ird')).toContain('model = "we\\"ird"');
  });
});

describe("SDK and generic snippets", () => {
  it("Python", () => {
    const t = openAiSdkPython(OPENAI, "burrow-medium");
    expect(t).toContain(`base_url="${OPENAI}"`);
    expect(t).toContain('api_key=os.environ["BURROW_GATEWAY_KEY"]');
    expect(t).toContain('model="burrow-medium"');
  });
  it("TypeScript", () => {
    const t = openAiSdkTypeScript(OPENAI, "burrow-medium");
    expect(t).toContain(`baseURL: "${OPENAI}"`);
    expect(t).toContain("apiKey: process.env.BURROW_GATEWAY_KEY");
    expect(t).toContain('model: "burrow-medium"');
  });
  it("generic form values", () => {
    expect(genericProvider(OPENAI, ["a", "b"])).toEqual([
      { label: "Base URL", value: OPENAI },
      { label: "API key", value: "<your gateway key>" },
      { label: "Models", value: "a, b" },
    ]);
  });
  it("no snippet contains something that looks like a real key", () => {
    const all = [
      claudeCodeSettings(ANTHROPIC, pickTier(["a"])), codexConfig(OPENAI, "a"),
      openAiSdkPython(OPENAI, "a"), openAiSdkTypeScript(OPENAI, "a"),
    ].join("\n");
    expect(all).not.toMatch(/bgw_[A-Za-z0-9_-]{8,}/);
  });
});
