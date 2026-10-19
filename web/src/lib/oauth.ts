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

// A private-use scheme of a native app (RFC 8252 7.1): a reverse domain name.
const PRIVATE_SCHEME = /^[a-z][a-z0-9+-]*(\.[a-z0-9+-]+)+$/;
const AUTHORITY = /^(\[[0-9a-fA-F:.]+\]|[^:[\]]*)(:\d*)?$/;

function isLoopbackHost(host: string): boolean {
  const h = host.toLowerCase();
  if (h === "localhost" || h === "[::1]") return true;
  const m = /^127\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
  return m !== null && m.slice(1).every((o) => Number(o) <= 255);
}

/**
 * Whether the relay accepts raw as a client's redirect address: https with a
 * host, http on a loopback address, or an app scheme with a dot
 * (com.example.app:/cb); no fragment, no user, at most 2048 characters. The
 * same rule as ValidRedirectURI in internal/oauth/redirect.go; the relay
 * decides in the end.
 */
export function validRedirectUri(raw: string): boolean {
  if (raw === "" || raw.length > 2048 || /\s/.test(raw) || raw.includes("#")) return false;
  const m = /^([a-zA-Z][a-zA-Z0-9+.-]*):(.*)$/.exec(raw);
  if (!m) return false;
  const scheme = m[1].toLowerCase();
  if (scheme !== "https" && scheme !== "http") return PRIVATE_SCHEME.test(scheme);
  const a = /^\/\/([^/?#]*)/.exec(m[2]);
  if (!a || a[1].includes("@")) return false;
  const auth = AUTHORITY.exec(a[1]);
  if (!auth) return false;
  const host = auth[1];
  return scheme === "https" ? host !== "" : isLoopbackHost(host);
}
