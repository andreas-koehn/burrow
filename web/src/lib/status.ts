import type { AiProvider, ClientView, Service } from "./contract";

/** Display text for a status value: lower-case words, underscores as spaces. */
export function statusLabel(raw: string | null | undefined): string {
  return (raw ?? "").toLowerCase().replace(/_/g, " ");
}

/** Anything served through a client: a service, a tunnel provider, a client. */
export type ServedState = "online" | "offline" | "never_connected";
/** A hosted provider: whether its credential is set. */
export type HostedState = "ready" | "not_configured";
export type State = ServedState | HostedState;

/** The one word for each state, as every page writes it. */
export const STATE_LABEL: Record<State, string> = {
  online: "Online",
  offline: "Offline",
  never_connected: "Never connected",
  ready: "Ready",
  not_configured: "Not configured",
};

/** Badge kind (index.css) of each state. */
export const STATE_BADGE: Record<State, string> = {
  online: "status-online",
  offline: "status-offline",
  never_connected: "status-never",
  ready: "status-online",
  not_configured: "status-never",
};

const SERVED: readonly string[] = ["online", "offline", "never_connected"];

/**
 * How many clients are online, given the live sessions (GET /clients). A client
 * is a token of a user: one that is connected twice counts once, as it is one
 * row of the Clients page.
 */
export function clientsOnline(sessions: Pick<ClientView, "user_id" | "token_name">[]): number {
  return new Set(sessions.map((s) => JSON.stringify([s.user_id, s.token_name]))).size;
}

/**
 * Works against a relay that predates `status`: falls back to `connected`,
 * which cannot tell offline from never connected.
 */
export function serviceState(s: Pick<Service, "status" | "connected">): ServedState {
  if (SERVED.includes(s.status)) return s.status;
  return s.connected ? "online" : "offline";
}

/** Works against a relay that predates `state`: falls back to `status` and the credential. */
export function providerState(p: Pick<AiProvider, "state" | "status" | "kind" | "credential_present">): State {
  if (Object.hasOwn(STATE_LABEL, p.state ?? "")) return p.state;
  if (p.kind === "direct") return p.credential_present ? "ready" : "not_configured";
  return p.status === "Offline" ? "offline" : "online";
}
