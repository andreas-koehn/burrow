import type { OAuthGrant } from "./contract";

/** The scopes of /mcp/burrow that the read-only switch stands for. */
export const SCOPE_READ = "burrow:read";
export const SCOPE_WRITE = "burrow:write";

/** True when the request names only the two broad scopes, so that one switch can say it. */
export function isBroadScopes(scopes: string[]): boolean {
  return scopes.every((s) => s === SCOPE_READ || s === SCOPE_WRITE);
}

/** What an address in the grant means to a person: the path of the resource decides. */
export function resourceLabel(resource: string, sourceName?: string): string {
  let path = resource;
  try {
    path = new URL(resource).pathname;
  } catch {
    // Not an absolute URL: take it as the path.
  }
  path = path.replace(/\/+$/, "");
  if (path === "/mcp/burrow") return "Burrow management";
  if (path === "/mcp") return "All tool sources";
  const m = /^\/mcp\/([^/]+)$/.exec(path);
  return m ? `Tool source ${sourceName ?? m[1]}` : resource;
}

/** One line on what a connected app may do. */
export function grantAccess(g: Pick<OAuthGrant, "scopes" | "allowed_tools" | "tools_read_only">): string {
  if (g.scopes.includes("mcp:tools")) {
    const tools = g.allowed_tools.length === 0 ? "no tools" : g.allowed_tools.join(", ");
    return `${tools}${g.tools_read_only ? ", read-only tools only" : ""}`;
  }
  if (g.scopes.includes(SCOPE_WRITE)) return "Full access";
  if (g.scopes.length === 1 && g.scopes[0] === SCOPE_READ) return "Read-only";
  return g.scopes.join(", ");
}
