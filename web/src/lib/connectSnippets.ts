// Ready configurations for the clients the connect card offers. Pure text
// builders: the card only renders what these return, and none of them ever
// takes a key — the key is a placeholder or an environment variable.
//
// Checked against the vendors' documentation (2026-10):
// - Claude Code reads ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN (sent as
//   "Authorization: Bearer") from the env block of ~/.claude/settings.json, and
//   ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL pin the model per tier.
//   https://code.claude.com/docs/en/llm-gateway-connect, …/model-config
//   Its gateway model discovery keeps only ids that contain "claude" or
//   "anthropic", so names like burrow-medium are pinned here instead.
//   https://code.claude.com/docs/en/llm-gateway-protocol#model-discovery
// - Codex declares a provider under [model_providers.<id>] with name, base_url,
//   env_key and wire_api; "responses" is the only wire_api it supports.
//   https://developers.openai.com/codex/config-reference

/** What stands where the gateway key goes. */
export const KEY_PLACEHOLDER = "<your gateway key>";
/** Environment variable the Codex and SDK examples read the key from. */
export const KEY_ENV = "BURROW_GATEWAY_KEY";

export interface Tiers {
  opus?: string;
  sonnet?: string;
  haiku?: string;
}

/** Spreads models over Claude Code's three tiers in list order; the last one fills what is left. */
export function pickTier(models: string[]): Tiers {
  if (models.length === 0) return {};
  const at = (i: number) => models[Math.min(i, models.length - 1)]!;
  return { opus: at(0), sonnet: at(1), haiku: at(2) };
}

/** The env block of Claude Code's ~/.claude/settings.json. */
export function claudeCodeSettings(baseUrl: string, tiers: Tiers): string {
  const env: Record<string, string> = {
    ANTHROPIC_BASE_URL: baseUrl,
    ANTHROPIC_AUTH_TOKEN: KEY_PLACEHOLDER,
  };
  if (tiers.opus) env.ANTHROPIC_DEFAULT_OPUS_MODEL = tiers.opus;
  if (tiers.sonnet) env.ANTHROPIC_DEFAULT_SONNET_MODEL = tiers.sonnet;
  if (tiers.haiku) env.ANTHROPIC_DEFAULT_HAIKU_MODEL = tiers.haiku;
  return JSON.stringify({ env }, null, 2);
}

function tomlString(s: string): string {
  return `"${s.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

/** A provider block for Codex's ~/.codex/config.toml. */
export function codexConfig(baseUrl: string, model: string): string {
  return [
    `model = ${tomlString(model)}`,
    'model_provider = "burrow"',
    "",
    "[model_providers.burrow]",
    'name = "Burrow"',
    `base_url = ${tomlString(baseUrl)}`,
    `env_key = ${tomlString(KEY_ENV)}`,
    'wire_api = "responses"',
  ].join("\n");
}

export function openAiSdkPython(baseUrl: string, model: string): string {
  return [
    "import os",
    "from openai import OpenAI",
    "",
    "client = OpenAI(",
    `    base_url=${JSON.stringify(baseUrl)},`,
    `    api_key=os.environ["${KEY_ENV}"],`,
    ")",
    "reply = client.chat.completions.create(",
    `    model=${JSON.stringify(model)},`,
    '    messages=[{"role": "user", "content": "Hello"}],',
    ")",
    "print(reply.choices[0].message.content)",
  ].join("\n");
}

export function openAiSdkTypeScript(baseUrl: string, model: string): string {
  return [
    'import OpenAI from "openai";',
    "",
    "const client = new OpenAI({",
    `  baseURL: ${JSON.stringify(baseUrl)},`,
    `  apiKey: process.env.${KEY_ENV},`,
    "});",
    "const reply = await client.chat.completions.create({",
    `  model: ${JSON.stringify(model)},`,
    '  messages: [{ role: "user", content: "Hello" }],',
    "});",
    "console.log(reply.choices[0].message.content);",
  ].join("\n");
}

/** The values a generic "OpenAI-compatible provider" form asks for. */
export function genericProvider(baseUrl: string, models: string[]): { label: string; value: string }[] {
  return [
    { label: "Base URL", value: baseUrl },
    { label: "API key", value: KEY_PLACEHOLDER },
    { label: "Models", value: models.join(", ") },
  ];
}
