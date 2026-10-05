import type { LucideIcon } from "lucide-react";
import { KeyRound, Waypoints } from "lucide-react";
import { NAVIGATIONS, allEntries, type NavContext, type NavEntry } from "./navigation";

export interface Destination {
  path: string;
  label: string;
  /** The workspace label: "Services" | "AI Gateway" | "Settings". */
  group: string;
  icon: LucideIcon;
}

/**
 * Temporary: two Services pages that have no entry in the navigation description
 * because W04 folds them into Services (`/services?live=1`) and Clients
 * (`/clients?tab=tokens`). Until then the sidebar appends them to the Connect
 * group and the palette lists them, so neither page disappears. W04 removes this.
 *
 * The Settings-area pages need no such block: every one of them already has an
 * entry in the navigation description, and `NOT_YET_MOVED` (lib/moved-routes.ts)
 * leads that entry to the page's current address until W03 and W05 move it.
 */
export const TEMPORARY_ENTRIES: NavEntry[] = [
  { id: "tunnels", label: "Tunnels", to: "/tunnels", icon: Waypoints },
  { id: "tokens", label: "Tokens", to: "/tokens", icon: KeyRound },
];

/** The command palette's static list: the navigation description, flattened. */
export function destinationsFor(ctx: NavContext): Destination[] {
  const out: Destination[] = [];
  const push = (entry: NavEntry, group: string) => out.push({ path: entry.to, label: entry.label, group, icon: entry.icon });
  let workspace: string | undefined;
  for (const item of allEntries(ctx)) {
    // The temporary entries close the Services block.
    if (workspace === "services" && item.workspace !== "services") {
      for (const e of TEMPORARY_ENTRIES) push(e, NAVIGATIONS.services.label);
    }
    workspace = item.workspace;
    push(item.entry, NAVIGATIONS[item.workspace].label);
  }
  return out;
}
