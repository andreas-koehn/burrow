import { useQuery } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import type { ClientView, KnownClient } from "@/lib/contract";

/**
 * One key for every reader, so the Clients page, the sidebar and the overview
 * share one request. It sits below ["clients"], the live sessions: invalidating
 * those invalidates this list too.
 */
const KNOWN_CLIENTS_KEY = ["clients", "known"] as const;

/** A live session as a row of its own: all a relay without /clients/known can say. */
function fromSession(s: ClientView): KnownClient {
  return {
    token_id: "", token_name: s.token_name, user_id: s.user_id, status: "online", last_seen: null,
    os: s.os, arch: s.arch, client_version: s.client_version, remote_addr: s.remote_addr,
    served: [], sessions: [s],
  };
}

/**
 * A relay that predates the list answers 404; its live sessions are then listed
 * instead, so the dashboard stays usable. Any other failure, a refusal included,
 * is the caller's to show.
 */
async function fetchKnownClients(): Promise<KnownClient[]> {
  try {
    return await apiFetch<KnownClient[]>("/clients/known");
  } catch (e) {
    if (!(e instanceof ApiError) || e.status !== 404) throw e;
    const live = await apiFetch<ClientView[]>("/clients");
    return Array.isArray(live) ? live.map(fromSession) : [];
  }
}

export interface KnownClientsOptions {
  /** The relay lists clients for an admin only: a reader that knows better does not ask. */
  enabled?: boolean;
  /** Milliseconds between two requests; without it the list is not refreshed on its own. */
  refetchInterval?: number;
}

/**
 * Every client the relay knows, one row per client token, in the relay's order
 * (GET /clients/known). These are the rows of the Clients tab; whoever counts
 * clients counts them, so that no figure disagrees with that page. `rows` is
 * undefined until a list has arrived.
 */
export function useKnownClients({ enabled = true, refetchInterval }: KnownClientsOptions = {}) {
  const { data, isLoading, isSuccess, error } = useQuery({
    queryKey: KNOWN_CLIENTS_KEY,
    queryFn: fetchKnownClients,
    retry: false,
    enabled,
    refetchInterval,
  });
  return { rows: Array.isArray(data) ? data : undefined, isLoading, isSuccess, error };
}

/** How many clients are online: the rows the Clients tab shows as Online. */
export function clientsOnline(rows: KnownClient[]): number {
  return rows.filter((c) => c.status === "online").length;
}
