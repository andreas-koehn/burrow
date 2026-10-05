import type { LucideIcon } from "lucide-react";
import {
  LayoutDashboard, Globe, Boxes, Activity, Sparkles, ShieldAlert, Database, Search, DollarSign,
  ServerCog, Mail, Archive, HardDrive, DatabaseBackup, Users, ShieldCheck, ScrollText, Webhook,
  BookOpen, UserCircle, MonitorSmartphone, Bot, Network,
} from "lucide-react";
import { workspaceFor, type Workspace } from "./workspace";

export interface NavEntry {
  id: string;
  label: string;
  /** Path the entry opens. */
  to: string;
  icon: LucideIcon;
  adminOnly?: boolean;
  /** Extra path prefixes for which this entry counts as the current page. */
  also?: string[];
  /** Live figure shown at the right edge of the entry. */
  count?: "services" | "clientsOnline";
  /** A shortcut into a whole area: it is marked for any page that needs attention, and then leads there. */
  gathersAttention?: boolean;
}

export interface NavGroup {
  title?: string;
  entries: NavEntry[];
}

export interface Navigation {
  workspace: Workspace;
  /** "Services" | "AI Gateway" | "Settings" */
  label: string;
  /** The relay namespace the workspace manages: "/svc/…" | "/ai/…". */
  namespace?: string;
  icon: LucideIcon;
  /** "/" | "/gateway" | "/settings" */
  home: string;
  groups: NavGroup[];
}

export interface NavContext {
  isAdmin: boolean;
  hasAiGateway: boolean;
}

/** Where the user chip leads. Named, so the sidebar cannot lose it to a renamed id. */
export const PROFILE_ENTRY: NavEntry = {
  id: "profile", label: "Profile & password", to: "/settings/profile", icon: UserCircle,
};

/**
 * The one description of what is where. Sidebar, breadcrumb, command palette and the
 * orphan-page test all read it; new pages go here, never into the sidebar component.
 */
export const NAVIGATIONS: Record<Workspace, Navigation> = {
  services: {
    workspace: "services", label: "Services", namespace: "/svc/…", icon: Network, home: "/",
    groups: [
      { entries: [{ id: "services-overview", label: "Overview", to: "/", icon: LayoutDashboard }] },
      { title: "Connect", entries: [
        { id: "services", label: "Services", to: "/services", icon: Globe, count: "services" },
        { id: "clients", label: "Clients", to: "/clients", icon: Boxes, count: "clientsOnline" },
      ] },
      { title: "Observe", entries: [{ id: "traffic", label: "Traffic", to: "/traffic", icon: Activity }] },
    ],
  },
  gateway: {
    workspace: "gateway", label: "AI Gateway", namespace: "/ai/…", icon: Sparkles, home: "/gateway",
    groups: [
      { entries: [{ id: "gateway-overview", label: "Overview", to: "/gateway", icon: LayoutDashboard }] },
      { title: "Route", entries: [{ id: "providers", label: "Providers", to: "/gateway/providers", icon: Sparkles }] },
      { title: "Control", entries: [
        { id: "guardrails", label: "Guardrails", to: "/gateway/guardrails", icon: ShieldAlert },
        { id: "cache", label: "Prompt cache", to: "/gateway/cache", icon: Database },
      ] },
      { title: "Observe", entries: [
        { id: "requests", label: "Requests", to: "/gateway/requests", icon: Search },
        { id: "cost", label: "Cost & budgets", to: "/gateway/cost", icon: DollarSign },
      ] },
    ],
  },
  settings: {
    workspace: "settings", label: "Settings", icon: ServerCog, home: "/settings",
    groups: [
      { title: "Relay", entries: [
        { id: "general", label: "General", to: "/settings/general", icon: ServerCog, adminOnly: true },
        { id: "email", label: "Email", to: "/settings/email", icon: Mail, adminOnly: true },
        { id: "retention", label: "Retention", to: "/settings/retention", icon: Archive, adminOnly: true },
        { id: "database", label: "Database", to: "/settings/database", icon: HardDrive, adminOnly: true },
        { id: "backups", label: "Backups", to: "/settings/backups", icon: DatabaseBackup, adminOnly: true },
      ] },
      { title: "Access", entries: [
        { id: "users", label: "Users", to: "/settings/users", icon: Users, adminOnly: true },
        { id: "roles", label: "Roles", to: "/settings/roles", icon: ShieldCheck, adminOnly: true },
        { id: "audit", label: "Audit log", to: "/settings/audit", icon: ScrollText, adminOnly: true },
      ] },
      { title: "Integrations", entries: [
        { id: "webhooks", label: "Webhooks", to: "/settings/webhooks", icon: Webhook, adminOnly: true },
        { id: "api", label: "API reference", to: "/settings/api", icon: BookOpen, adminOnly: true },
      ] },
      { title: "Personal", entries: [
        PROFILE_ENTRY,
        { id: "sessions", label: "Sessions", to: "/settings/sessions", icon: MonitorSmartphone },
        { id: "automation", label: "Automation tokens", to: "/settings/automation", icon: Bot },
      ] },
    ],
  },
};

/** Shortcuts into Settings shown at the foot of the sidebar; admin only. */
export const FOOTER_ENTRIES: NavEntry[] = [
  { id: "footer-users", label: "Users & roles", to: "/settings/users", icon: Users, adminOnly: true },
  { id: "footer-settings", label: "Settings", to: "/settings/general", icon: ServerCog, adminOnly: true, gathersAttention: true },
];

/**
 * The page behind this entry that needs attention, if any. `pages` are the
 * paths where open relay notices are resolved, most urgent first.
 */
export function attentionTarget(entry: NavEntry, pages: string[]): string | undefined {
  if (pages.includes(entry.to)) return entry.to;
  return entry.gathersAttention ? pages[0] : undefined;
}

/** The navigation of a workspace as this user sees it; groups left empty are dropped. */
export function navigationFor(workspace: Workspace, ctx: NavContext): Navigation {
  const nav = NAVIGATIONS[workspace];
  const groups = nav.groups
    .map((g) => ({ ...g, entries: g.entries.filter((e) => !e.adminOnly || ctx.isAdmin) }))
    .filter((g) => g.entries.length > 0);
  return { ...nav, groups };
}

/** The workspaces the switcher offers. */
export function workspacesFor(ctx: NavContext): Navigation[] {
  return ctx.hasAiGateway ? [NAVIGATIONS.services, NAVIGATIONS.gateway] : [NAVIGATIONS.services];
}

function covers(entry: NavEntry, home: string, pathname: string): boolean {
  if (entry.to === home) return pathname === home || pathname === home + "/";
  return [entry.to, ...(entry.also ?? [])].some((p) => pathname === p || pathname.startsWith(p + "/"));
}

/** The entry that counts as "you are here": the longest matching prefix. */
export function activeEntry(pathname: string, nav: Navigation): NavEntry | undefined {
  let best: NavEntry | undefined;
  for (const g of nav.groups) {
    for (const e of g.entries) {
      if (covers(e, nav.home, pathname) && (!best || e.to.length > best.to.length)) best = e;
    }
  }
  return best;
}

/** Pages that sit under an entry without being one of its objects. */
const SUBPAGES: Record<string, string> = {
  "/clients/connect": "Connect a client",
};

function decoded(segment: string): string {
  try { return decodeURIComponent(segment); } catch { return segment; }
}

/**
 * Workspace, entry, and — on a detail page — the object named by the next path segment.
 * `names` maps an object's id to what people call it (a service's or client's name); without
 * an entry the id itself is shown. A page below an object (a single request of a service)
 * adds a last crumb and turns the object into a link.
 */
export function breadcrumbFor(
  pathname: string,
  ctx: NavContext,
  names: Record<string, string> = {},
): { label: string; to?: string }[] {
  const nav = navigationFor(workspaceFor(pathname), ctx);
  const crumbs: { label: string; to?: string }[] = [{ label: nav.label, to: nav.home }];
  const entry = activeEntry(pathname, nav);
  if (!entry) return crumbs;
  const rest = pathname.slice(entry.to.length).split("/").filter(Boolean);
  if (rest.length === 0 || entry.to === nav.home) {
    crumbs.push({ label: entry.label });
    return crumbs;
  }
  crumbs.push({ label: entry.label, to: entry.to });
  const under = `${entry.to}/${rest[0]}`;
  const id = decoded(rest[0]);
  const label = SUBPAGES[under] ?? names[id] ?? id;
  if (rest.length === 1 || SUBPAGES[under]) {
    crumbs.push({ label });
    return crumbs;
  }
  crumbs.push({ label, to: under });
  crumbs.push({ label: decoded(rest[rest.length - 1]) });
  return crumbs;
}

/** Every entry this user can open, for the command palette. */
export function allEntries(ctx: NavContext): { entry: NavEntry; workspace: Workspace; group?: string }[] {
  const out: { entry: NavEntry; workspace: Workspace; group?: string }[] = [];
  const spaces: Workspace[] = ctx.hasAiGateway ? ["services", "gateway", "settings"] : ["services", "settings"];
  for (const ws of spaces) {
    for (const g of navigationFor(ws, ctx).groups) {
      for (const entry of g.entries) out.push({ entry, workspace: ws, group: g.title });
    }
  }
  return out;
}
