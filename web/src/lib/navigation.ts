import type { LucideIcon } from "lucide-react";
import {
  LayoutDashboard, Globe, Boxes, Activity, Sparkles, ShieldAlert, Database, Search, DollarSign,
  ServerCog, Mail, Archive, HardDrive, DatabaseBackup, Users, ShieldCheck, ScrollText, Webhook,
  BookOpen, UserCircle, MonitorSmartphone, Bot, Network, Layers, KeyRound, Cable,
  Wrench,
} from "lucide-react";
import { P } from "@/auth/permissions";
import { workspaceFor, type Workspace } from "./workspace";

export interface NavEntry {
  id: string;
  label: string;
  /** What the command palette calls the entry, where its label alone names two pages ("Overview"). */
  paletteLabel?: string;
  /** Path the entry opens. */
  to: string;
  icon: LucideIcon;
  /** Permissions that open the page (any of them); without, everybody sees the entry. They are those of the API the page reads. */
  needs?: readonly string[];
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
  /** The relay namespace the workspace manages: "/svc/…" | "/ai/…". Shown on the Overview, not in the shell. */
  namespace?: string;
  icon: LucideIcon;
  /** "/" | "/gateway" | "/settings" */
  home: string;
  groups: NavGroup[];
}

export interface NavContext {
  /** Whether the caller holds any of the permissions (none listed: yes). */
  can: (needs?: readonly string[]) => boolean;
  hasAiGateway: boolean;
}

/** Permissions shared by several entries, and by the routes that guard the same pages. */
const NEEDS = {
  // The page lists clients (tunnels:read:any) and, for everyone, the tokens and the connect flow (tokens:manage:*).
  clients: [P.tunnelsReadAny, P.tokensManageOwn, P.tokensManageAny],
  // Each is the permission set of the route the page loads (routeAccess in internal/api/access.go).
  services: [P.servicesConfigureOwn, P.servicesConfigureAny],
  traffic: [P.auditRead],
  providers: [P.aiReadOwn, P.aiReadAny, P.aiProvidersManage],
  tools: [P.mcpSourcesRead, P.mcpSourcesManage],
  aiRead: [P.aiReadOwn, P.aiReadAny, P.aiConfigureOwn, P.aiConfigureAny],
  requests: [P.inspectorReadOwn, P.inspectorReadAny],
  settings: [P.settingsManage],
  users: [P.usersRead, P.usersManage],
  roles: [P.rolesRead, P.rolesManage],
  // Personal pages still read an API that needs a permission: a custom role may lack it.
  sessions: [P.sessionsManageOwn, P.sessionsManageAny],
  automation: [P.automationTokensManageOwn, P.automationTokensManageAny],
} as const;

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
      { entries: [{ id: "services-overview", label: "Overview", paletteLabel: "Services overview", to: "/", icon: LayoutDashboard }] },
      { title: "Connect", entries: [
        { id: "services", label: "Services", to: "/services", icon: Globe, count: "services", needs: NEEDS.services },
        { id: "clients", label: "Clients", to: "/clients", icon: Boxes, count: "clientsOnline", needs: NEEDS.clients },
      ] },
      { title: "Observe", entries: [{ id: "traffic", label: "Traffic", to: "/traffic", icon: Activity, needs: NEEDS.traffic }] },
    ],
  },
  gateway: {
    workspace: "gateway", label: "AI Gateway", namespace: "/ai/…", icon: Sparkles, home: "/gateway",
    groups: [
      { entries: [{ id: "gateway-overview", label: "Overview", paletteLabel: "AI Gateway overview", to: "/gateway", icon: LayoutDashboard }] },
      // A client serves services and providers alike: the one page at /clients is listed in both workspaces.
      { title: "Connect", entries: [
        { id: "gateway-clients", label: "Clients", to: "/clients", icon: Boxes, count: "clientsOnline", needs: NEEDS.clients },
      ] },
      { title: "Route", entries: [
        { id: "providers", label: "Providers", to: "/gateway/providers", icon: Sparkles, needs: NEEDS.providers },
        { id: "tools", label: "Tools", to: "/gateway/tools", icon: Wrench, needs: NEEDS.tools },
        { id: "models", label: "Models", to: "/gateway/models", icon: Layers, needs: NEEDS.aiRead },
        { id: "gateway-keys", label: "Gateway keys", to: "/gateway/keys", icon: KeyRound, needs: NEEDS.aiRead },
      ] },
      { title: "Control", entries: [
        { id: "guardrails", label: "Guardrails", to: "/gateway/guardrails", icon: ShieldAlert, needs: NEEDS.aiRead },
        { id: "cache", label: "Prompt cache", to: "/gateway/cache", icon: Database, needs: NEEDS.aiRead },
      ] },
      { title: "Observe", entries: [
        { id: "requests", label: "Requests", to: "/gateway/requests", icon: Search, needs: NEEDS.requests },
        { id: "cost", label: "Cost & budgets", to: "/gateway/cost", icon: DollarSign },
      ] },
    ],
  },
  settings: {
    workspace: "settings", label: "Settings", icon: ServerCog, home: "/settings",
    groups: [
      { title: "Relay", entries: [
        { id: "general", label: "General", to: "/settings/general", icon: ServerCog, needs: NEEDS.settings },
        { id: "email", label: "Email", to: "/settings/email", icon: Mail, needs: NEEDS.settings },
        { id: "retention", label: "Retention", to: "/settings/retention", icon: Archive, needs: NEEDS.settings },
        { id: "database", label: "Database", to: "/settings/database", icon: HardDrive, needs: [P.metricsRead] },
        { id: "backups", label: "Backups", to: "/settings/backups", icon: DatabaseBackup, needs: [P.backupRun] },
      ] },
      { title: "Access", entries: [
        { id: "users", label: "Users", to: "/settings/users", icon: Users, needs: NEEDS.users },
        { id: "roles", label: "Roles", to: "/settings/roles", icon: ShieldCheck, needs: NEEDS.roles },
        { id: "audit", label: "Audit log", to: "/settings/audit", icon: ScrollText, needs: [P.auditRead] },
      ] },
      { title: "Integrations", entries: [
        { id: "webhooks", label: "Webhooks", to: "/settings/webhooks", icon: Webhook, needs: [P.webhooksManage] },
        { id: "mcp", label: "MCP", to: "/settings/mcp", icon: Cable, needs: [P.mcpToolsRead] },
        { id: "api", label: "API reference", to: "/settings/api", icon: BookOpen, needs: [P.metricsRead] },
      ] },
      { title: "Personal", entries: [
        PROFILE_ENTRY,
        { id: "sessions", label: "Sessions", to: "/settings/sessions", icon: MonitorSmartphone, needs: NEEDS.sessions },
        { id: "automation", label: "Automation tokens", to: "/settings/automation", icon: Bot, needs: NEEDS.automation },
      ] },
    ],
  },
};

/** Shortcuts into Settings shown at the foot of the sidebar, to those who may open the page. */
export const FOOTER_ENTRIES: NavEntry[] = [
  { id: "footer-users", label: "Users & roles", to: "/settings/users", icon: Users, needs: NEEDS.users },
  { id: "footer-settings", label: "Settings", to: "/settings/general", icon: ServerCog, needs: NEEDS.settings, gathersAttention: true },
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
    .map((g) => ({ ...g, entries: g.entries.filter((e) => ctx.can(e.needs)) }))
    .filter((g) => g.entries.length > 0);
  return { ...nav, groups };
}

/** The workspaces the tabs at the head of the sidebar offer. */
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
 * adds a last crumb and turns the object into a link. `last` is the workspace a relay-level
 * page (Clients) is shown in; left out, it is the one remembered.
 */
export function breadcrumbFor(
  pathname: string,
  ctx: NavContext,
  names: Record<string, string> = {},
  last?: "services" | "gateway",
): { label: string; to?: string }[] {
  const nav = navigationFor(workspaceFor(pathname, last), ctx);
  const crumbs: { label: string; to?: string }[] = [{ label: nav.label, to: nav.home }];
  const entry = activeEntry(pathname, nav);
  if (!entry) return crumbs;
  // The workspace and its entry of the same name would read "Services / Services":
  // the entry is kept, since it is the page an object below it goes back to.
  if (entry.label === nav.label) crumbs.pop();
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

/**
 * Every page this user can open, for the command palette. A page listed in two
 * workspaces (Clients) appears once, as the first entry that opens it.
 */
export function allEntries(ctx: NavContext): { entry: NavEntry; workspace: Workspace; group?: string }[] {
  const out: { entry: NavEntry; workspace: Workspace; group?: string }[] = [];
  const seen = new Set<string>();
  const spaces: Workspace[] = ctx.hasAiGateway ? ["services", "gateway", "settings"] : ["services", "settings"];
  for (const ws of spaces) {
    for (const g of navigationFor(ws, ctx).groups) {
      for (const entry of g.entries) {
        if (seen.has(entry.to)) continue;
        seen.add(entry.to);
        out.push({ entry, workspace: ws, group: g.title });
      }
    }
  }
  return out;
}
