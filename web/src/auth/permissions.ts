import type { Me } from "./useAuth";

/** Every permission, as `authz.AllPermissions()` lists them (internal/authz/authz.go); a test keeps the two equal. */
export const ALL_PERMISSIONS: readonly string[] = [
  "tunnels:read:own",
  "tunnels:read:any",
  "tunnels:manage:own",
  "tunnels:manage:any",
  "tokens:manage:own",
  "tokens:manage:any",
  "services:configure:own",
  "services:configure:any",
  "sessions:manage:own",
  "sessions:manage:any",
  "users:read",
  "users:manage",
  "roles:read",
  "settings:manage",
  "ai:read:own",
  "ai:read:any",
  "ai:configure:own",
  "ai:configure:any",
  "quotas:read:own",
  "quotas:read:any",
  "quotas:manage:any",
  "inspector:read:own",
  "inspector:read:any",
  "inspector:replay:own",
  "inspector:replay:any",
  "audit:read",
  "webhooks:manage",
  "roles:manage",
  "automation:tokens:manage:own",
  "automation:tokens:manage:any",
  "backup:run",
  "metrics:read",
  "mcp:tools:read",
  "mtls:manage:own",
  "mtls:manage:any",
  "ipgeo:manage:own",
  "ipgeo:manage:any",
  "ai:providers:manage",
  "budgets:manage",
  "cost:pricing:manage",
];

/** The built-in "user" role (`userPerms` in internal/authz/authz.go). */
export const USER_PERMISSIONS: readonly string[] = [
  "tunnels:read:own",
  "tunnels:manage:own",
  "tokens:manage:own",
  "services:configure:own",
  "sessions:manage:own",
  "ai:read:own",
  "ai:configure:own",
  "quotas:read:own",
  "inspector:read:own",
  "inspector:replay:own",
  "automation:tokens:manage:own",
  "mtls:manage:own",
  "ipgeo:manage:own",
];

/** Named permissions, so a page asks for `P.auditRead` and a typo is a compile error. */
export const P = {
  tunnelsReadOwn: "tunnels:read:own",
  tunnelsReadAny: "tunnels:read:any",
  tunnelsManageOwn: "tunnels:manage:own",
  tunnelsManageAny: "tunnels:manage:any",
  tokensManageOwn: "tokens:manage:own",
  tokensManageAny: "tokens:manage:any",
  servicesConfigureOwn: "services:configure:own",
  servicesConfigureAny: "services:configure:any",
  sessionsManageOwn: "sessions:manage:own",
  sessionsManageAny: "sessions:manage:any",
  usersRead: "users:read",
  usersManage: "users:manage",
  rolesRead: "roles:read",
  settingsManage: "settings:manage",
  aiReadOwn: "ai:read:own",
  aiReadAny: "ai:read:any",
  aiConfigureOwn: "ai:configure:own",
  aiConfigureAny: "ai:configure:any",
  quotasReadOwn: "quotas:read:own",
  quotasReadAny: "quotas:read:any",
  quotasManageAny: "quotas:manage:any",
  inspectorReadOwn: "inspector:read:own",
  inspectorReadAny: "inspector:read:any",
  inspectorReplayOwn: "inspector:replay:own",
  inspectorReplayAny: "inspector:replay:any",
  auditRead: "audit:read",
  webhooksManage: "webhooks:manage",
  rolesManage: "roles:manage",
  automationTokensManageOwn: "automation:tokens:manage:own",
  automationTokensManageAny: "automation:tokens:manage:any",
  backupRun: "backup:run",
  metricsRead: "metrics:read",
  mcpToolsRead: "mcp:tools:read",
  mtlsManageOwn: "mtls:manage:own",
  mtlsManageAny: "mtls:manage:any",
  ipgeoManageOwn: "ipgeo:manage:own",
  ipgeoManageAny: "ipgeo:manage:any",
  aiProvidersManage: "ai:providers:manage",
  budgetsManage: "budgets:manage",
  costPricingManage: "cost:pricing:manage",
} as const;

export type PermissionKey = keyof typeof P;

/** What the caller may do: what `/me` says, else (a relay from before /me carried it) by role. */
export function permissionsOf(me: Me | undefined): ReadonlySet<string> {
  if (!me) return new Set();
  if (Array.isArray(me.permissions)) return new Set(me.permissions);
  // A relay from before /me carried permissions: admin held everything, every other role at most the user set.
  return new Set(me.role === "admin" ? ALL_PERMISSIONS : USER_PERMISSIONS);
}

/** Any-of; no needs (or an empty list) is open to everybody. */
export function holdsAny(held: ReadonlySet<string>, needs?: readonly string[]): boolean {
  return !needs || needs.length === 0 || needs.some((p) => held.has(p));
}
