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
  // Tunnels are the Live filter of Services; client tokens are a tab of Clients.
  { from: "/tunnels", to: "/services?live=1" },
  { from: "/tokens", to: "/clients?tab=tokens" },
  // Connection logs are the Traffic view of the Services workspace.
  { from: "/connection-logs", to: "/traffic" },
  // Custom domains need host routing, which is off; its retired settings page lands on General.
  { from: "/settings/custom-domains", to: "/settings/general" },
];
