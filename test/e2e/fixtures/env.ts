// test-only — never deploy this shape.
// MUST match test/harness/compose.full.yml. Change one, change both.

import { spawnSync } from "node:child_process";

export const ADMIN_EMAIL = "admin@e2e.local";
export const ADMIN_PASSWORD = "e2e-pass";

export const DASHBOARD_URL = "http://localhost:8080";

// HTTPS proxy listener (wildcard cert). HTTP tunnels are HOST-ROUTED here — they
// have NO fixed --remote port. To target a specific http tunnel, send a request
// to HTTPS_INGRESS with the host header set to "<subdomain>.test.local"; the
// relay's proxy uses the host header to route to the registered tunnel.
//
// Plan-fidelity note: the plan-as-written assumed a fixed `TUNNEL_AI_URL =
// http://localhost:9001`. That binding does not exist — http-type tunnels
// only bind via the host-routed proxy on :8443.
export const HTTPS_INGRESS = "https://localhost:8443";

// TCP tunnels DO bind a fixed --remote port.
export const TUNNEL_TCP_URL = "http://localhost:9002";   // client-tcp echo
export const TUNNEL_MULTI_A = "http://localhost:9003";   // client-multi svc-a
export const TUNNEL_MULTI_B = "http://localhost:9004";   // client-multi svc-b

export const TUNNEL_NAMES = ["ai", "tcp-echo", "svc-a", "svc-b"] as const;

export const COMPOSE_FILE = "test/harness/compose.full.yml";
export const COMPOSE_POSTGRES_OVERRIDE = "test/harness/compose.postgres.yml";

export const RESET_URL = `${DASHBOARD_URL}/api/v1/internal/test-reset`;

// Discovers the AI service's slug by querying the services API.
// Uses the session cookie from playwright-auth.json for authentication.
// HTTP tunnels register a random subdomain at session start. Cached after first
// call — call resetAiSubdomainCache() when the tunnel is known to have re-registered
// (e.g. after composeRestartRelay()).
//
// Strategy: query GET /api/v1/services (authenticated) and find the service with
// name === "ai" that is currently connected. This is authoritative and immune to
// stale relay-log entries left by ephemeral tunnels from spec 33 (token-connect).
let _aiSubdomain: string | undefined;
export function aiSubdomain(): string {
  if (_aiSubdomain) return _aiSubdomain;

  // Try the API approach: GET /api/v1/services with the saved session cookie.
  // spawnSync curl.exe is available on Windows and POSIX; we use the system curl.
  try {
    const storagePath = "playwright-auth.json";
    const { readFileSync } = require("node:fs") as typeof import("node:fs");
    const storage = JSON.parse(readFileSync(storagePath, "utf8")) as {
      cookies: { name: string; value: string }[];
    };
    const sessionCookie = storage.cookies?.find((c) => c.name === "burrow_session")?.value ?? "";
    if (sessionCookie) {
      const result = spawnSync(
        "curl",
        ["-sk", "http://localhost:8080/api/v1/services", "-H", `Cookie: burrow_session=${sessionCookie}`],
        { encoding: "utf8" },
      );
      if (result.status === 0 && result.stdout) {
        const services = JSON.parse(result.stdout) as {
          name: string;
          slug: string;
          connected: boolean;
        }[];
        const ai = services.find((s) => s.name === "ai" && s.connected && s.slug);
        if (ai) {
          _aiSubdomain = ai.slug;
          return _aiSubdomain;
        }
      }
    }
  } catch {
    // Fall through to log-based approach.
  }

  // Fallback: parse relay docker logs. Note: this may resolve to a stale ephemeral
  // subdomain if other tunnels registered after the AI tunnel. Only used when the
  // API approach fails (e.g. before spec 01 runs).
  const logs = spawnSync(
    "docker",
    ["logs", "burrow-e2e-full-relay-1"],
    { encoding: "utf8" },
  );
  const combined = (logs.stdout ?? "") + (logs.stderr ?? "");
  const lines = combined.split("\n").filter((l) => l.includes("http tunnel registered"));
  const out = lines[lines.length - 1] ?? "";
  const m = out.match(/slug=([a-z0-9-]+)/);
  if (!m) throw new Error(`aiSubdomain: relay log has no "http tunnel registered" line yet`);
  _aiSubdomain = m[1];
  return _aiSubdomain;
}
export function resetAiSubdomainCache(): void {
  _aiSubdomain = undefined;
}
export function aiHost(): string {
  return `${aiSubdomain()}.test.local`;
}
