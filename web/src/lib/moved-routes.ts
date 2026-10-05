/**
 * Paths that moved. Rendered as routes in App.tsx with `RedirectTo`; listed here so tests
 * can walk them. (Kept apart from the component so fast refresh keeps working.)
 */
export const OLD_ROUTES: { from: string; to: string }[] = [
  { from: "/cache", to: "/gateway/cache" },
  { from: "/guardrails", to: "/gateway/guardrails" },
  { from: "/inspector", to: "/gateway/requests" },
  { from: "/inspector/:serviceId/:requestId?", to: "/gateway/requests/:serviceId/:requestId?" },
  { from: "/cost", to: "/gateway/cost" },
  { from: "/users", to: "/settings/users" },
  { from: "/roles", to: "/settings/roles" },
  { from: "/audit", to: "/settings/audit" },
  { from: "/webhooks", to: "/settings/webhooks" },
  { from: "/openapi", to: "/settings/api" },
  { from: "/account", to: "/settings/profile" },
  { from: "/account/automation", to: "/settings/automation" },
  // Custom domains need host routing, which is off; its retired settings page lands on General.
  { from: "/settings/custom-domains", to: "/settings/general" },
];

/**
 * Temporary, and the other way round: paths the navigation already names whose
 * pages still live at their old address. Rendered as routes in App.tsx so that
 * no sidebar, footer or palette entry leads nowhere. W05 removes `/traffic`
 * (and adds the reverse row to OLD_ROUTES). The reachability test in
 * navigation.test.ts pins this list against its own PENDING array.
 */
export const NOT_YET_MOVED: { from: string; to: string }[] = [
  { from: "/traffic", to: "/connection-logs" },
];
