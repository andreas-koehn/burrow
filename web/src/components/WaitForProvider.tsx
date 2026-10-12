import { useEffect, useRef } from "react";
import type { ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { StatusBadge } from "@/components/StatusBadge";
import { providerState } from "@/lib/status";
import { KNOWN_CLIENTS_KEY } from "@/lib/useKnownClients";
import type { AiProvider } from "@/lib/contract";

// How often a provider that is online is asked again for a catalog it does not have yet.
const CATALOG_ANSWERS = 5;

export interface WaitForProviderProps {
  slug: string;
  /** Shown below the line once the provider is online: what to do next. */
  children?: ReactNode;
}

/**
 * The line a reader waits on after the command: it asks for the provider
 * every two seconds until a client serves it, then says Online and how many
 * models the catalog has. An answer that fails ends the asking.
 */
export function WaitForProvider({ slug, children }: WaitForProviderProps) {
  const qc = useQueryClient();
  // The number of the answer that first said online.
  const onlineSince = useRef<number | null>(null);
  const provider = useQuery({
    // Not below ["ai", "providers"]: refreshing the list must not ask here again.
    queryKey: ["ai", "provider-status", slug],
    queryFn: () => apiFetch<AiProvider>(`/ai/providers/${encodeURIComponent(slug)}`),
    retry: false,
    // An answer from an earlier wait says nothing about now.
    gcTime: 0,
    refetchInterval: (query) => {
      if (query.state.status === "error") return false;
      const p = query.state.data;
      if (!p || providerState(p) !== "online") {
        onlineSince.current = null;
        return 2000;
      }
      if (p.model_count > 0) return false;
      // The catalog is read after the client has connected: ask a little longer for it.
      onlineSince.current ??= query.state.dataUpdateCount;
      return query.state.dataUpdateCount - onlineSince.current < CATALOG_ANSWERS ? 2000 : false;
    },
  });
  const p = provider.data;
  const online = p !== undefined && providerState(p) === "online";

  // The lists behind this line are not asked every two seconds: tell them once.
  useEffect(() => {
    if (!online) return;
    void qc.invalidateQueries({ queryKey: KNOWN_CLIENTS_KEY });
    void qc.invalidateQueries({ queryKey: ["ai", "providers"] });
  }, [online, qc]);

  return (
    <>
      <div role="status" className="connect-wait">
        {online ? (
          <>
            <StatusBadge state="online" />
            {p.model_count > 0 && <span>{p.model_count === 1 ? "1 model found" : `${p.model_count} models found`}</span>}
          </>
        ) : provider.isError ? (
          <span className="muted">The status could not be read. The provider's page shows it.</span>
        ) : (
          <>
            <span className="spinner" aria-hidden="true" />
            <span className="muted">Waiting for the client…</span>
          </>
        )}
      </div>
      {online && children}
    </>
  );
}
