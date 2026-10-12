import { useId } from "react";
import { Link } from "react-router-dom";
import { InstallLines, type InstallLineId } from "@/components/InstallLines";
import { ServedBy } from "@/components/ServedBy";
import { MODEL_SERVER_PORT, isCommandName, runTarget } from "@/lib/installSnippets";
import { serviceState } from "@/lib/status";
import type { Service } from "@/lib/contract";

const RUN_ONLY: readonly InstallLineId[] = ["run"];

/**
 * Who serves a service: the client by name while it is online; otherwise who
 * served it last and the command that starts it. Installing and signing in are
 * left to the page the link opens. Whoever holds the service asks for it again
 * while it is not online, so the line turns Online by itself.
 */
export function ServiceClientPanel({ service }: { service: Service }) {
  const titleId = useId();
  const state = serviceState(service);
  const online = state === "online";
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const connect = `/clients/connect?service=${encodeURIComponent(service.id)}`;
  // A service that backs a provider gets the port its provider page shows.
  const target = runTarget(service.local_addr, service.provider_slug ? MODEL_SERVER_PORT : undefined);
  // `burrow http` serves HTTP services only, and finds the one it serves by its name.
  const http = service.type === "http";
  const named = isCommandName(service.name);

  return (
    <section className="card col gap-2" aria-labelledby={titleId}>
      <h2 id={titleId}>Client</h2>
      {/* A relay that predates these fields leaves them out. */}
      <ServedBy state={state} client={service.last_client ?? ""} sessionId={service.client_session_id ?? ""} lastSeen={service.last_seen} />
      {!online && !(http && named) && (
        <p className="muted small">
          {http
            ? "Its name cannot stand in a command."
            : <>This is not an HTTP service: <code>burrow http</code> does not serve it.</>}{" "}
          <Link className="link-inline" to={connect}>Connect a client</Link> shows other ways to serve it.
        </p>
      )}
      {!online && http && named && (
        <>
          <p className="muted small">
            {state === "offline"
              ? "Run this on the machine to bring the service back online."
              : "Run this on a machine that is signed in, where the service listens."}
          </p>
          <InstallLines relayOrigin={origin} target={target} name={service.name} lines={RUN_ONLY} />
          <p className="small">
            <Link className="link-inline" to={connect}>Connect a client</Link>
          </p>
        </>
      )}
    </section>
  );
}
