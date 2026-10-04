export const GLOSSARY = {
  client: "A machine running `burrow connect` that exposes one or more local services through this relay.",
  tunnel: "The live forwarding from a client's local port to a public URL — it exists only while the client is connected.",
  service: "The durable saved configuration (access mode + URL) that a tunnel exposes; it persists even when no client is connected.",
  endpoint: "An AI endpoint: a Service with API-key access and an OpenAI-compatible upstream, surfaced under AI Gateway.",
  customDomain: "A CNAME + TLS certificate pair you attach to a service so it answers on your own hostname.",
  clientToken: "Authenticates a machine running `burrow connect` (a tunneling client).",
  automationToken: "A long-lived bearer token for CI, the CLI, or bots — scoped to your own permissions; not for tunneling clients.",
} as const;
