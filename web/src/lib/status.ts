/** Display text for a status value: lower-case words, underscores as spaces. */
export function statusLabel(raw: string | null | undefined): string {
  return (raw ?? "").toLowerCase().replace(/_/g, " ");
}
