import type { LucideIcon } from "lucide-react";
import { NAVIGATIONS, allEntries, type NavContext, type NavEntry } from "./navigation";

export interface Destination {
  path: string;
  label: string;
  /** The workspace label: "Services" | "AI Gateway" | "Settings". */
  group: string;
  icon: LucideIcon;
}

/** The command palette's static list: the navigation description, flattened. */
export function destinationsFor(ctx: NavContext): Destination[] {
  const out: Destination[] = [];
  const push = (entry: NavEntry, group: string) => out.push({ path: entry.to, label: entry.label, group, icon: entry.icon });
  for (const item of allEntries(ctx)) push(item.entry, NAVIGATIONS[item.workspace].label);
  return out;
}
