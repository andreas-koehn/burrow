/** Display text for a status value: lower-case words, underscores as spaces. */
export function statusLabel(raw: string): string {
  return raw.toLowerCase().replace(/_/g, " ");
}
