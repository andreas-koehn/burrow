/** The tool allow-list entry rules of a gateway key (docs/openapi.yaml, AIGatewayKey.allowed_tools). */

export const MAX_TOOL_ENTRIES = 64;
export const MAX_TOOL_ENTRY_LENGTH = 160;

/** "<source>__<tool>" or "<source>__*"; the source is a slug of 1-24 characters of a-z, 0-9 and "-". */
const ENTRY = /^[a-z0-9-]{1,24}__\S+$/;

/** An empty string when the entry is usable, else the reason. */
export function toolEntryProblem(entry: string): string {
  if (entry.length > MAX_TOOL_ENTRY_LENGTH) return `An entry is at most ${MAX_TOOL_ENTRY_LENGTH} characters.`;
  const at = entry.indexOf("__");
  const tool = at < 0 ? "" : entry.slice(at + 2);
  // A "*" stands for all tools of the source and nothing else: no "src__read_*".
  if (!ENTRY.test(entry) || (tool.includes("*") && tool !== "*")) {
    return "Use <source>__<tool> for one tool or <source>__* for all tools of a source. The source is a slug (a-z, 0-9, -).";
  }
  return "";
}

/** Whether two lists hold the same entries in the same order. */
export function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}
