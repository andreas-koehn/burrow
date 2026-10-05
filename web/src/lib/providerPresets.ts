export interface ProviderPreset {
  id: "openrouter" | "zai-coding" | "zai-api" | "custom";
  label: string;
  slug: string;
  name: string;
  baseUrl: string;
  credentialSlot: string;
  billing: "metered" | "flat";
  note?: string;
}

/** Starting points for the "New provider" dialog. Every field stays editable. */
export const PROVIDER_PRESETS: ProviderPreset[] = [
  {
    id: "openrouter", label: "OpenRouter", slug: "openrouter", name: "OpenRouter",
    baseUrl: "https://openrouter.ai/api/v1", credentialSlot: "OPENROUTER", billing: "metered",
    note: "Cost is taken from what OpenRouter reports per request.",
  },
  {
    id: "zai-coding", label: "z.ai — Coding Plan", slug: "zai", name: "z.ai",
    baseUrl: "https://api.z.ai/api/coding/paas/v4", credentialSlot: "ZAI", billing: "flat",
    note: "Subscription endpoint. The plan's quota is shared by everyone using this provider.",
  },
  {
    id: "zai-api", label: "z.ai — API (pay as you go)", slug: "zai", name: "z.ai",
    baseUrl: "https://api.z.ai/api/paas/v4", credentialSlot: "ZAI", billing: "metered",
  },
  {
    id: "custom", label: "Other OpenAI-compatible API", slug: "", name: "",
    baseUrl: "", credentialSlot: "", billing: "metered",
  },
];

/** Same rule as the relay's vault slots. */
export const CREDENTIAL_SLOT_RE = /^[A-Z0-9_]{1,32}$/;
export const CREDENTIAL_SLOT_HINT = "1–32 characters: A–Z, 0–9 and underscore.";

/** Environment variable the relay reads a credential slot from. */
export function envVarForSlot(slot: string): string {
  return `BURROW_UPSTREAM_KEY_${slot}`;
}
