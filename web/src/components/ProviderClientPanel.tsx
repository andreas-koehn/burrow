import { useEffect, useId, useRef } from "react";
import { Link } from "react-router-dom";
import { InstallLines, type InstallLineId } from "@/components/InstallLines";
import { ServedBy } from "@/components/ServedBy";
import { WaitForProvider } from "@/components/WaitForProvider";
import { MODEL_SERVER_PORT, isCommandName, runTarget } from "@/lib/installSnippets";
import { providerState } from "@/lib/status";
import { useKnownClients } from "@/lib/useKnownClients";
import type { AiProvider, Service } from "@/lib/contract";

export interface ProviderClientPanelProps {
  /** A provider of kind tunnel: a hosted API has no client. */
  provider: AiProvider;
  /** Its backing service: the command names it, and it names the live session. */
  service: Service;
  /** The relay lists its clients for an admin only. */
  isAdmin: boolean;
  /** Called once when a client starts serving the provider while the panel waits for one. */
  onOnline?: () => void;
}

const RUN_ONLY: readonly InstallLineId[] = ["run"];

/** Rendered once the provider is online: tells whoever holds the provider to read it again. */
function Arrived({ onArrive }: { onArrive?: () => void }) {
  // The newest callback, called once: a parent that builds a new one on every render is not called again.
  const latest = useRef(onArrive);
  useEffect(() => {
    latest.current = onArrive;
  });
  useEffect(() => {
    latest.current?.();
  }, []);
  return null;
}

/**
 * Who serves a provider that runs on a machine: the client by name while it is
 * online; otherwise since when it is gone and the command that starts it. The
 * command names the backing service, because that is what a client serves.
 */
export function ProviderClientPanel({ provider, service, isAdmin, onOnline }: ProviderClientPanelProps) {
  const titleId = useId();
  const state = providerState(provider);
  const online = state === "online";
  // Offline means a client served it before; anything else that none has yet.
  const never = !online && state !== "offline";
  // Only a provider nobody served yet may need a machine set up first.
  const known = useKnownClients({ enabled: isAdmin && never });

  // A relay that predates these fields leaves them out.
  const client = provider.last_client || service.last_client || "";
  const session = service.client_session_id || provider.client_session_id || "";
  const lastSeen = provider.last_seen ?? service.last_seen ?? null;

  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const target = runTarget(service.local_addr, MODEL_SERVER_PORT);
  const hasClient = (known.rows?.length ?? 0) > 0;
  const allLines = never && !hasClient;
  const connect = `/clients/connect?provider=${encodeURIComponent(provider.slug)}`;

  return (
    <section className="card col gap-2" aria-labelledby={titleId}>
      <h2 id={titleId}>Client</h2>
      <ServedBy state={online ? "online" : never ? "never_connected" : "offline"} client={client} sessionId={session} lastSeen={lastSeen} />
      {!online && !isCommandName(service.name) && (
        <p className="muted small">
          The name of its service cannot stand in a command.{" "}
          <Link className="link-inline" to={connect}>Connect a client</Link> shows other ways to serve it.
        </p>
      )}
      {!online && isCommandName(service.name) && (
        <>
          <p className="muted small">
            {allLines
              ? "Run these on the machine where the model server listens."
              : never
                ? "Run this on a machine that is signed in, where the model server listens."
                : "Run this on the machine to bring the provider back online."}
          </p>
          {/* Until the list of clients has arrived it is not known which lines are needed. */}
          {!known.isLoading && (
            <InstallLines relayOrigin={origin} target={target} name={service.name} lines={allLines ? undefined : RUN_ONLY} />
          )}
          {/* It may have been offline for days: the relay is still asked, but only a provider nobody served yet
              says that it waits. The line above turns Online either way. */}
          <WaitForProvider
            slug={provider.slug}
            quiet={!never}
            failureText="The status could not be read: this page no longer updates by itself."
          >
            <Arrived onArrive={onOnline} />
          </WaitForProvider>
          {!known.isLoading && !allLines && (
            <p className="small">
              <Link className="link-inline" to={connect}>Connect another machine</Link>
            </p>
          )}
        </>
      )}
    </section>
  );
}
