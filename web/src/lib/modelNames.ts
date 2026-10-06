export const MODEL_NAME_RE = /^[a-z0-9][a-z0-9._-]{1,62}$/;
const SLUG_RE = /^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$/;
const ENTRY_HINT = "Use a model name, provider/model, or provider/*.";

/** Message for an invalid synthetic model name, or null. Empty is "not chosen yet". */
export function modelNameError(value: string): string | null {
  if (value === "") return null;
  if (value === "v1") return '"v1" is reserved.';
  if (!MODEL_NAME_RE.test(value)) {
    return "2–63 characters: lowercase letters, digits, dot, underscore and hyphen. No slash.";
  }
  return null;
}

/** Message for an invalid allow-list entry, or null. */
export function allowEntryError(value: string): string | null {
  if (value === "") return ENTRY_HINT;
  const i = value.indexOf("/");
  if (i < 0) return modelNameError(value);
  const provider = value.slice(0, i);
  const model = value.slice(i + 1);
  if (!SLUG_RE.test(provider) || provider === "v1" || model === "") return ENTRY_HINT;
  if (model !== "*" && (model.includes("*") || /\s/.test(model))) return ENTRY_HINT;
  return null;
}
