import type { LucideIcon } from "lucide-react";
import {
  LayoutDashboard,
  Boxes,
  Waypoints,
  Globe,
  KeyRound,
  Sparkles,
  DollarSign,
  Database,
  ShieldAlert,
  Search,
  Users,
  ShieldCheck,
  ServerCog,
  ScrollText,
  Webhook,
  UserCircle,
  Bot,
} from "lucide-react";

export interface Destination {
  path: string;
  label: string;
  group: string;
  icon: LucideIcon;
  adminOnly?: boolean;
  needsAiGroup?: boolean;
  needsHttpService?: boolean;
}

export const DESTINATIONS: Destination[] = [
  { path: "/", label: "Home", group: "Overview", icon: LayoutDashboard },
  { path: "/clients", label: "Clients", group: "Tunneling", icon: Boxes },
  { path: "/tunnels", label: "Tunnels", group: "Tunneling", icon: Waypoints },
  { path: "/services", label: "Services", group: "Tunneling", icon: Globe },
  { path: "/tokens", label: "Tokens", group: "Tunneling", icon: KeyRound },
  { path: "/gateway/providers", label: "Providers", group: "AI Gateway", icon: Sparkles, needsAiGroup: true },
  { path: "/cost", label: "Cost & budgets", group: "AI Gateway", icon: DollarSign, needsAiGroup: true },
  { path: "/cache", label: "Prompt cache", group: "AI Gateway", icon: Database, needsAiGroup: true },
  { path: "/guardrails", label: "Guardrails", group: "AI Gateway", icon: ShieldAlert, needsAiGroup: true },
  { path: "/inspector", label: "Request inspector", group: "AI Gateway", icon: Search, needsAiGroup: true, needsHttpService: true },
  { path: "/users", label: "Users", group: "Access control", icon: Users, adminOnly: true },
  { path: "/roles", label: "Roles", group: "Access control", icon: ShieldCheck, adminOnly: true },
  { path: "/settings", label: "Settings", group: "Administration", icon: ServerCog, adminOnly: true },
  { path: "/audit", label: "Audit", group: "Administration", icon: ScrollText, adminOnly: true },
  { path: "/webhooks", label: "Webhooks", group: "Administration", icon: Webhook, adminOnly: true },
  { path: "/account", label: "Account", group: "Account", icon: UserCircle },
  { path: "/account/automation", label: "Automation", group: "Account", icon: Bot },
];

export function destinationsFor(ctx: {
  isAdmin: boolean;
  hasAiGateway: boolean;
  firstHttpServiceId?: string;
}): Destination[] {
  return DESTINATIONS
    .filter((d) => !d.adminOnly || ctx.isAdmin)
    .filter((d) => !d.needsAiGroup || ctx.hasAiGateway)
    .filter((d) => !d.needsHttpService || !!ctx.firstHttpServiceId);
}
