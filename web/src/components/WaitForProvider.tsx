import { useEffect, useRef } from "react";
import type { ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { Button } from "@/components/ds";
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
  /**
   * What to say when the status cannot be read. The default points to the
   * provider's page, so that page itself gives its own words.
   */
  failureText?: string;
  /**
   * Asks without saying that it waits, and leaves saying Online to whoever it
   * hands over to. For a provider that may stay offline for days: a line that
   * waits would spin for as long.
   */
  quiet?: boolean;
}

/**
 * The line a reader waits on after the command: it asks for the provider
 * every two seconds until a client serves it, then says Online and how many
 * models the catalog has. An answer that fails ends the asking until the
 * reader asks again.
 */
export function WaitForProvider({
  slug, children, quiet = false,
  failureText = "The status could not be read. The provider's page shows it.",
}: WaitForProviderProps) {
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
  // Not while the question is asked again: then it waits, as before the failure.
  const failed = !online && provider.isError && !provider.isFetching;

  // The lists behind this line are not asked every two seconds: tell them once.
  useEffect(() => {
    if (!online) return;
    void qc.invalidateQueries({ queryKey: KNOWN_CLIENTS_KEY });
    void qc.invalidateQueries({ queryKey: ["ai", "providers"] });
  }, [online, qc]);

  const said = failed ? (
    <span className="muted">{failureText}</span>
  ) : quiet ? null : online ? (
    <>
      <StatusBadge state="online" />
      {p.model_count > 0 && <span>{p.model_count === 1 ? "1 model found" : `${p.model_count} models found`}</span>}
    </>
  ) : (
    <>
      <span className="spinner" aria-hidden="true" />
      <span className="muted">Waiting for the client…</span>
    </>
  );

  return (
    <>
      {/* With nothing to say the line takes no room, but stays: what arrives in it later is announced. */}
      <div className={said === null ? "visually-hidden" : "connect-wait"}>
        <div role="status" className="row row-center gap-2">{said}</div>
        {/* Beside the message, not in it: a button is not part of what is announced. */}
        {failed && (
          <Button variant="secondary" size="sm" onClick={() => void provider.refetch()}>Try again</Button>
        )}
      </div>
      {online && children}
    </>
  );
}
