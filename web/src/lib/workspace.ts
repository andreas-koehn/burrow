export type Workspace = "services" | "gateway" | "settings";

const KEY = "burrow.lastWorkspace";

function under(pathname: string, prefix: string): boolean {
  return pathname === prefix || pathname.startsWith(prefix + "/");
}

/** The workspace a path belongs to. The sidebar shows nothing else. */
export function workspaceFor(pathname: string): Workspace {
  if (under(pathname, "/gateway")) return "gateway";
  if (under(pathname, "/settings")) return "settings";
  return "services";
}

/** Remembers where "Back to …" in Settings should lead. */
export function rememberWorkspace(pathname: string): void {
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
