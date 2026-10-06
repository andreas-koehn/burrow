export interface ProviderPreset {
  id: "openrouter" | "zai-coding" | "zai-api" | "custom";
  label: string;
  slug: string;
  name: string;
  baseUrl: string;
  credentialSlot: string;
  billing: "metered" | "flat";
  /** Whether the provider documents POST /responses (the OpenAI Responses API). */
  supportsResponses: boolean;
  note?: string;
}

/** Starting points for the "New provider" dialog. Every field stays editable. */
export const PROVIDER_PRESETS: ProviderPreset[] = [
  {
    id: "openrouter", label: "OpenRouter", slug: "openrouter", name: "OpenRouter",
    baseUrl: "https://openrouter.ai/api/v1", credentialSlot: "OPENROUTER", billing: "metered",
    supportsResponses: true,
    note: "Cost is taken from what OpenRouter reports per request.",
  },
  {
    id: "zai-coding", label: "z.ai — Coding Plan", slug: "zai", name: "z.ai",
    baseUrl: "https://api.z.ai/api/coding/paas/v4", credentialSlot: "ZAI", billing: "flat",
    supportsResponses: false,
    note: "Subscription endpoint. The plan's quota is shared by everyone using this provider.",
  },
  {
    id: "zai-api", label: "z.ai — API (pay as you go)", slug: "zai", name: "z.ai",
    baseUrl: "https://api.z.ai/api/paas/v4", credentialSlot: "ZAI", billing: "metered",
    supportsResponses: false,
  },
  {
    id: "custom", label: "Other OpenAI-compatible API", slug: "", name: "",
    baseUrl: "", credentialSlot: "", billing: "metered",
    supportsResponses: false,
  },
];

// Same rule as the relay's vault slots.
const CREDENTIAL_SLOT_RE = /^[A-Z0-9_]{1,32}$/;

/** Local validation message for a credential slot name, or null when it is fine or empty. */
export function credentialSlotError(slot: string): string | null {
  if (slot === "") return null;
  if (!CREDENTIAL_SLOT_RE.test(slot)) return "1–32 characters: A–Z, 0–9 and underscore.";
  // BURROW_UPSTREAM_KEY_FOO_FILE is the path of a file holding slot FOO's
  // key, so a slot named FOO_FILE could never be told apart from it.
  if (slot.endsWith("_FILE")) {
    return `A slot name cannot end in _FILE: the relay reads ${envVarForSlot(slot)} as the path of a key file for slot ${slot.slice(0, -5) || "…"}.`;
  }
  return null;
}

/** Environment variable the relay reads a credential slot from. */
export function envVarForSlot(slot: string): string {
  return `BURROW_UPSTREAM_KEY_${slot}`;
}

/** Help text of the "Requests at once" field of a provider. */
export const CONCURRENCY_HELP =
  "How many requests this provider serves in parallel. More wait for a free place. Leave empty for no limit — set it for a local model on one GPU.";

/** Shown while the "Requests at once" field holds something that is not a whole number. */
export const CONCURRENCY_FORMAT = "Enter a whole number, or leave empty for no limit.";

/**
 * Reads the "Requests at once" field: empty and 0 mean no limit (0). null when
 * the text is not a whole number. The range is the server's to check.
 */
export function parseConcurrency(text: string): number | null {
  const t = text.trim();
  if (t === "") return 0;
  return /^\d{1,9}$/.test(t) ? Number(t) : null;
}
