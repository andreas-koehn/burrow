import type { InspectorEntry } from "@/lib/contract";

/** A request the tool gateway served for a tools/call; every other request, MCP or not, is a model request. */
export const isToolCall = (r: Pick<InspectorEntry, "mcp">): boolean => r.mcp?.method === "tools/call";

/** The tool a call named: the served name the gateway recorded. */
export const toolOf = (r: Pick<InspectorEntry, "mcp" | "model">): string => r.mcp?.tool || r.model || "";

export interface ToolResult {
  /** Pretty JSON of the result object, "" when the body held none. */
  text: string;
  isError: boolean;
}

function pretty(v: unknown): string {
  return JSON.stringify(v, null, 2) ?? "";
}

/** The arguments of a captured tools/call: the params' "arguments" member, else the params as sent. */
export function toolArguments(params: unknown): string {
  if (params && typeof params === "object" && "arguments" in params) return pretty((params as { arguments: unknown }).arguments);
  return params === undefined ? "" : pretty(params);
}

/** The result of a captured answer (a JSON-RPC response, possibly an SSE-less JSON body). */
export function toolResult(body: string): ToolResult {
  try {
    const msg = JSON.parse(body) as { result?: { isError?: boolean }; error?: unknown };
    if (msg.result !== undefined) return { text: pretty(msg.result), isError: msg.result?.isError === true };
    if (msg.error !== undefined) return { text: pretty(msg.error), isError: true };
  } catch { /* not JSON: shown as it is below */ }
  return { text: body, isError: false };
}
