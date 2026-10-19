/** The tool allow-list entry rules of a gateway key (docs/openapi.yaml, AIGatewayKey.allowed_tools). */

export const MAX_TOOL_ENTRIES = 64;
export const MAX_TOOL_ENTRY_LENGTH = 160;


const RESERVED = new Set(["burrow", "v1"]);
const hasControl = (s: string) => Array.from(s).some((c) => c.charCodeAt(0) < 0x20 || c.charCodeAt(0) === 0x7f);
const SOURCE = /^[a-z0-9-]{1,24}$/;
const bytes = (s: string) => new TextEncoder().encode(s).length;

/** An empty string when the entry is usable (the store's ValidToolEntry), else the reason. */
export function toolEntryProblem(entry: string): string {
  if (bytes(entry) > MAX_TOOL_ENTRY_LENGTH) return `An entry is at most ${MAX_TOOL_ENTRY_LENGTH} bytes.`;
  const at = entry.indexOf("__");
  const source = at < 0 ? "" : entry.slice(0, at);
  const tool = at < 0 ? "" : entry.slice(at + 2);
  // A "*" stands for all tools of the source and nothing else: no "src__read_*".
  const toolOk = tool === "*" || (tool !== "" && !/[* ]/.test(tool) && !hasControl(tool));
  if (!SOURCE.test(source) || RESERVED.has(source) || !toolOk) {
    return "Use <source>__<tool> for one tool or <source>__* for all tools of a source. The source is a slug (a-z, 0-9, -), not burrow or v1.";
  }
  return "";
}

/** Whether two lists hold the same entries in the same order. */
export function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}
