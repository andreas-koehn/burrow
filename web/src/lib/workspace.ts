export type Workspace = "services" | "gateway" | "settings";

const KEY = "burrow.lastWorkspace";

function under(pathname: string, prefix: string): boolean {
  return pathname === prefix || pathname.startsWith(prefix + "/");
}

/** Paths that belong to the relay, not to one workspace: a client serves services and providers alike. */
export function isShared(pathname: string): boolean {
  return under(pathname, "/clients");
}

/**
 * The workspace a path is shown in; the sidebar shows nothing else. It follows the URL,
 * except on a relay-level page, which stays in the workspace used last.
 */
export function workspaceFor(pathname: string, last: "services" | "gateway" = lastWorkspace()): Workspace {
  if (under(pathname, "/gateway")) return "gateway";
  if (under(pathname, "/settings")) return "settings";
  if (isShared(pathname)) return last;
  return "services";
}

/** Remembers where "Back to …" in Settings should lead, and which workspace a relay-level page is shown in. */
export function rememberWorkspace(pathname: string): void {
  if (isShared(pathname)) return;
  const ws = workspaceFor(pathname);
  if (ws === "settings") return;
  try { localStorage.setItem(KEY, ws); } catch { /* storage unavailable: the default applies */ }
}

export function lastWorkspace(): "services" | "gateway" {
  try {
    return localStorage.getItem(KEY) === "gateway" ? "gateway" : "services";
  } catch {
    return "services";
  }
}
