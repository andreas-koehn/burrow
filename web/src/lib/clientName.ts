const MAX = 63;

/** Validation message for a client name, or null when it is valid. */
export function clientNameError(name: string): string | null {
  if (name === "") return "Enter a name.";
  if (name.length > MAX) return `Use at most ${MAX} characters.`;
  if (!/^[a-z0-9-]+$/.test(name)) return "Use lowercase letters, digits and hyphens only.";
  if (name.startsWith("-") || name.endsWith("-")) return "Start and end with a letter or digit.";
  return null;
}
