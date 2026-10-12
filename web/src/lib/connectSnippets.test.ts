import { describe, it, expect } from "vitest";
import {
  claudeCodeSettings, codexConfig, curlChat, openAiEnv, openAiSdkPython, openAiSdkTypeScript, genericProvider, pickTier, KEY_ENV,
} from "./connectSnippets";

const ANTHROPIC = "https://b.example.com/anthropic";
const OPENAI = "https://b.example.com/openai/v1";
const PROVIDER = "https://b.example.com/ai/ollama/v1";

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
});

describe("curlChat", () => {
  it("is a chat completion that sends the key as a bearer token", () => {
    expect(curlChat(PROVIDER, "qwen2.5:0.5b")).toBe(
      [
        `curl ${PROVIDER}/chat/completions \\`,
        '  -H "Authorization: Bearer $BURROW_GATEWAY_KEY" \\',
        '  -H "Content-Type: application/json" \\',
        `  -d '{"model": "qwen2.5:0.5b", "messages": [{"role": "user", "content": "Hello"}]}'`,
      ].join("\n"),
    );
  });
});

describe("openAiEnv", () => {
  it("sets the two variables an OpenAI SDK reads, the key from the gateway key's variable", () => {
    expect(openAiEnv(PROVIDER)).toBe(
      [`export OPENAI_BASE_URL=${PROVIDER}`, "export OPENAI_API_KEY=$BURROW_GATEWAY_KEY"].join("\n"),
    );
  });
});

describe("where the key comes from", () => {
  // What a shell or a program runs: it can read a variable.
  const reading: [string, string][] = [
    ["Codex config", codexConfig(OPENAI, "a")],
    ["Python", openAiSdkPython(OPENAI, "a")],
    ["TypeScript", openAiSdkTypeScript(OPENAI, "a")],
    ["curl", curlChat(PROVIDER, "a")],
    ["environment variables", openAiEnv(PROVIDER)],
  ];
  // A settings file and a form field cannot: they name the key in a placeholder.
  const placeholder: [string, string][] = [
    ["Claude Code settings", claudeCodeSettings(ANTHROPIC, pickTier(["a"]))],
    ["generic form values", genericProvider(OPENAI, ["a"]).map((r) => r.value).join("\n")],
  ];

  it("the variable is BURROW_GATEWAY_KEY", () => {
    expect(KEY_ENV).toBe("BURROW_GATEWAY_KEY");
  });
  it.each(reading)("%s reads the key from BURROW_GATEWAY_KEY", (_name, text) => {
    expect(text).toContain("BURROW_GATEWAY_KEY");
    expect(text).not.toContain("<your gateway key>");
  });
  it.each(placeholder)("%s cannot read a variable and names the gateway key in a placeholder", (_name, text) => {
    expect(text).toContain("<your gateway key>");
    expect(text).not.toContain("BURROW_GATEWAY_KEY");
  });
  it.each([...reading, ...placeholder])("%s names no other Burrow variable and no real key", (_name, text) => {
    expect(text).not.toContain("BURROW_API_KEY");
    // The only variable that starts with BURROW_ is the gateway key's.
    expect(text.replace(/BURROW_GATEWAY_KEY/g, "")).not.toMatch(/BURROW_[A-Z_]+/);
    expect(text).not.toMatch(/\b(bgw|buk|bua)_[A-Za-z0-9_-]{8,}/);
  });
  it("a variable the app reads itself is set from it, never to a value", () => {
    const assigned = openAiEnv(PROVIDER).split("\n").find((l) => l.includes("OPENAI_API_KEY"));
    expect(assigned).toBe("export OPENAI_API_KEY=$BURROW_GATEWAY_KEY");
  });
});
