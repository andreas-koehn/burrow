export const MODEL_NAME_RE = /^[a-z0-9][a-z0-9._-]{1,62}$/;

/** Message for an invalid synthetic model name, or null. Empty is "not chosen yet". */
export function modelNameError(value: string): string | null {
  if (value === "") return null;
  if (value === "v1") return '"v1" is reserved.';
  if (!MODEL_NAME_RE.test(value)) {
    return "2–63 characters: lowercase letters, digits, dot, underscore and hyphen. No slash.";
  }
  return null;
}
