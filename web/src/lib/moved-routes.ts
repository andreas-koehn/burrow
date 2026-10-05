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
];

/**
 * Temporary, and the other way round: paths the navigation already names whose
 * pages still live at their old address. Rendered as routes in App.tsx so that
 * no sidebar, footer or palette entry leads nowhere. W03 removes the Settings
 * rows as it moves each page (and adds the reverse row to OLD_ROUTES); W05
 * removes `/traffic`. The reachability test in navigation.test.ts pins this
 * list against its own PENDING array.
 */
export const NOT_YET_MOVED: { from: string; to: string }[] = [
  { from: "/traffic", to: "/connection-logs" },
  { from: "/settings/general", to: "/settings" },
  { from: "/settings/email", to: "/settings" },
  { from: "/settings/users", to: "/users" },
  { from: "/settings/roles", to: "/roles" },
  { from: "/settings/audit", to: "/audit" },
  { from: "/settings/webhooks", to: "/webhooks" },
  { from: "/settings/api", to: "/openapi" },
  { from: "/settings/profile", to: "/account" },
  { from: "/settings/sessions", to: "/account" },
  { from: "/settings/automation", to: "/account/automation" },
];
