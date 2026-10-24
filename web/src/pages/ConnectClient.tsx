import { useEffect, useId, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, CircleCheck } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, PageHeader, ErrorNotice, Segmented } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { InstallLines } from "@/components/InstallLines";
import { WaitForProvider } from "@/components/WaitForProvider";
import { CHECKSUMS_HREF, EXAMPLE_TARGET, MODEL_SERVER_PORT, downloadTargets, isCommandName, runTarget } from "@/lib/installSnippets";
import { KNOWN_CLIENTS_KEY } from "@/lib/useKnownClients";
import type { AiProvider, ClientView, ClientDiscovery, Service } from "@/lib/contract";

interface ConnectInfo { server: string }

const BACK = { to: "/clients", label: "Clients" } as const;

function yamlExample(endpoint: string): string {
  return [
    `server: ${endpoint}`,
    "token_file: /etc/burrow/token",
    "services:",
    "  - name: web",
    "    local: 127.0.0.1:3000",
    "    type: http",
  ].join("\n");
}

/**
 * The page's live state below the three lines: it asks for the clients list
 * every two seconds and reports the first client that was not there when the
 * page opened. Only whoever holds tunnels:read:any can list clients; anyone else is told where to look.
 */
function WaitForClient() {
  const { loading } = useAuth();
  // GET /clients: tunnels:read:any.
  const canListClients = useCan(P.tunnelsReadAny);
  // The clients that were connected already, from the first answer after the page opened.
  const [known, setKnown] = useState<ReadonlySet<string> | null>(null);
  const [arrived, setArrived] = useState<ClientView | null>(null);

  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    // Once a client is here there is nothing left to ask.
    enabled: canListClients && !arrived,
    refetchInterval: (query) => (query.state.status === "error" ? false : 2000),
    retry: false,
  });

  // An answer from before this page opened (the Clients page shares the query) says nothing about now.
  if (canListClients && !arrived && clients.isFetchedAfterMount && Array.isArray(clients.data)) {
    if (known === null) {
      setKnown(new Set(clients.data.map((c) => c.session_id)));
    } else {
      const fresh = clients.data.find((c) => !known.has(c.session_id));
      if (fresh) setArrived(fresh);
    }
  }

  // The sidebar counts from the list of known clients, which this page does not
  // ask for: tell it once, so the count does not wait for the shell's next refresh.
  const qc = useQueryClient();
  const hasArrived = arrived !== null;
  useEffect(() => {
    if (hasArrived) void qc.invalidateQueries({ queryKey: KNOWN_CLIENTS_KEY });
  }, [hasArrived, qc]);

  if (loading) return null;
  return (
    <div role="status" className="connect-wait">
      {arrived ? (
        <>
          <CircleCheck size={16} aria-hidden="true" className="connect-wait-done" />
          <span>
            Connected: <Link className="link-inline" to={`/clients/${arrived.session_id}`}>{arrived.token_name}</Link>
          </span>
        </>
      ) : canListClients && !clients.isError ? (
        <>
          <span className="spinner" aria-hidden="true" />
          <span className="muted">Waiting for your client…</span>
        </>
      ) : (
        <span className="muted">Your client appears under Clients once it connects.</span>
      )}
    </div>
  );
}

const isGone = (e: unknown) => e instanceof ApiError && e.status === 404;

/** What the page is asked to bring online, from ?service=<id> or ?provider=<slug>. */
type Served =
  | { state: "none" | "loading" }
  /** Nothing to serve after all: the plain page, and why. */
  | { state: "plain"; note: string }
  /**
   * `target` is where the run line points; `leftOut` says that the service was
   * last served from an address that may not stand in a command.
   */
  | { state: "service"; name: string; target: string; leftOut: boolean; providerSlug?: string };

function useServed(): Served {
  const [params] = useSearchParams();
  const serviceParam = params.get("service");
  const providerParam = serviceParam === null ? params.get("provider") : null;
  const provider = useQuery({
    queryKey: ["connect", "provider", providerParam],
    queryFn: () => apiFetch<AiProvider>(`/ai/providers/${encodeURIComponent(providerParam ?? "")}`),
    retry: false,
    enabled: providerParam !== null,
  });
  // A hosted API has a service row too, but no machine behind it.
  const hosted = provider.data !== undefined && provider.data.kind !== "tunnel";
  const serviceId = serviceParam ?? (hosted ? null : provider.data?.service_id ?? null);
  const service = useQuery({
    queryKey: ["connect", "service", serviceId],
    queryFn: () => apiFetch<Service>(`/services/${encodeURIComponent(serviceId ?? "")}`),
    retry: false,
    enabled: serviceId !== null,
  });

  if (serviceParam === null && providerParam === null) return { state: "none" };
  if (hosted) return { state: "plain", note: "That provider is a hosted API: no machine serves it." };
  if (isGone(provider.error)) return { state: "plain", note: "That provider no longer exists." };
  if (isGone(service.error)) return { state: "plain", note: "That service no longer exists." };
  // Any other failure: the plain page serves as well as it did before.
  if (provider.isError || service.isError) return { state: "none" };
  const svc = service.data;
  if (svc === undefined) return { state: "loading" };
  // `burrow http` serves HTTP services only.
  if (svc.type !== "http") {
    return { state: "plain", note: "That service is not an HTTP service: the lines below do not serve it." };
  }
  // A client finds the service it serves by its name.
  if (!isCommandName(svc.name)) {
    return { state: "plain", note: "The name of that service cannot stand in a command: the lines below publish a new service." };
  }
  // Where it was last served from, when that may stand in a command. Without it a
  // model server gets the port its provider's page shows, anything else the example.
  const reported = (svc.local_addr ?? "").trim();
  const listens = runTarget(reported);
  const fallback = providerParam !== null || svc.provider_slug ? MODEL_SERVER_PORT : EXAMPLE_TARGET;
  return {
    state: "service",
    name: svc.name,
    target: listens ?? fallback,
    leftOut: reported !== "" && listens === undefined,
    providerSlug: providerParam ?? undefined,
  };
}

export default function ConnectClient() {
  const otherId = useId();
  const served = useServed();
  // What the machine serves: a web app or API (burrow http) or a gRPC server (burrow grpc).
  const [grpc, setGrpc] = useState(false);
  // What the reader chose; until then the section follows the relay's age.
  const [otherChoice, setOtherChoice] = useState<boolean | null>(null);

  // P1-2: control-plane endpoint from relay; fall back to window.location.host
  const ci = useQuery({
    queryKey: ["connect-info"],
    queryFn: () => apiFetch<ConnectInfo>("/clients/connect-info"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  // Tells the relay's version, and by its absence that the relay predates the three lines.
  const discovery = useQuery({
    queryKey: ["client-discovery"],
    queryFn: () => apiFetch<ClientDiscovery>("/client/discovery"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  const olderRelay = discovery.error instanceof ApiError && discovery.error.status === 404;
  const otherOpen = otherChoice ?? olderRelay;

  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const endpoint = ci.data?.server
    || discovery.data?.control
    || (typeof window !== "undefined" ? window.location.host : "relay.example.com");
  const downloads = discovery.data ? downloadTargets(discovery.data.version) : null;

  return (
    <div className="account-page page-narrow">
      <PageHeader
        back={BACK}
        title="Connect a client"
        subtitle="Bring a machine online so it can expose a local service through this Burrow relay."
      />

      {served.state === "service" && (
        <p className="page-intro">Serve <strong>{served.name}</strong> from this machine.</p>
      )}
      <p className="muted page-intro">
        A client is a machine running <code>burrow</code>. Run these three lines on it: they install the
        client from this relay, sign the machine in and publish a local port. Already connected?{" "}
        <Link className="link-inline" to="/clients?tab=tokens">Manage tokens</Link>.
      </p>

      {olderRelay && (
        <ErrorNotice variant="warn" role="status">
          This relay is older than the client commands below; use 'Other ways to connect'.
        </ErrorNotice>
      )}

      {served.state === "plain" && <p className="muted small">{served.note}</p>}
      {/* Said, not passed over: the port in the run line is then not where the service was last served from. */}
      {served.state === "service" && served.leftOut && (
        <p className="muted small">
          The address this service was last served from cannot stand in a command. The run line
          shows port {served.target} instead: put in where the server listens.
        </p>
      )}
      {served.state !== "loading" && (
        <Segmented
          aria-label="Kind of service"
          options={[{ value: "http", label: "Web app or API" }, { value: "grpc", label: "gRPC" }]}
          value={grpc ? "grpc" : "http"}
          onChange={(v) => setGrpc(v === "grpc")}
        />
      )}
      {/* While the answer is out no command is shown: it would be the one for some other service. */}
      {served.state !== "loading" && (
        <>
          <InstallLines
            relayOrigin={origin}
            target={served.state === "service" ? served.target : undefined}
            name={served.state === "service" ? served.name : undefined}
            grpc={grpc}
          />
          {served.state === "service" && served.providerSlug
            ? <WaitForProvider slug={served.providerSlug} />
            : <WaitForClient />}
        </>
      )}

      <section className="account-section" aria-labelledby={`${otherId}-head`}>
        <h2 id={`${otherId}-head`} className="connect-other-head">
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={otherOpen}
            aria-controls={`${otherId}-panel`}
            icon={otherOpen ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
            onClick={() => setOtherChoice(!otherOpen)}
          >
            Other ways to connect
          </Button>
        </h2>
        <div id={`${otherId}-panel`} hidden={!otherOpen}>
          {otherOpen && (
            <>
              <section className="account-section" aria-labelledby="ob-download">
                <div className="section-head"><div className="left"><h3 id="ob-download">Download by hand</h3></div></div>
                {downloads ? (
                  <>
                    <p className="muted small">
                      The same archives the installer fetches. Unpack one and put <code>burrow</code> on your <code>PATH</code>.
                    </p>
                    <ul role="list" className="connect-downloads">
                      {downloads.map((g) => (
                        <li key={g.os}>
                          <span className="connect-downloads-os">{g.label}</span>
                          {g.builds.map((b) => (
                            // A plain link: the relay answers this path, not the dashboard's router.
                            <a key={b.arch} className="link-inline" href={b.href} aria-label={`${g.label} ${b.arch}`}>{b.arch}</a>
                          ))}
                        </li>
                      ))}
                    </ul>
                    <p className="muted small">
                      Compare the archive's SHA-256 with <a className="link-inline" href={CHECKSUMS_HREF}>checksums.txt</a>{" "}
                      to see that the download is complete and undamaged.
                    </p>
                  </>
                ) : olderRelay ? (
                  <p className="muted small">
                    This relay does not hand out the client. Take the <code>burrow</code> archive from the same release as the relay.
                  </p>
                ) : discovery.isError ? (
                  // 429, a relay error or no network: not an old relay, so asking again can help.
                  <p className="muted small row row-center gap-2">
                    <span>Could not ask the relay which builds it offers.</span>
                    <Button variant="secondary" size="sm" disabled={discovery.isFetching} onClick={() => void discovery.refetch()}>
                      Try again
                    </Button>
                  </p>
                ) : (
                  <p className="muted small">Asking the relay which builds it offers…</p>
                )}
              </section>

              <section className="account-section" aria-labelledby="ob-yaml">
                <div className="section-head"><div className="left"><h3 id="ob-yaml">burrow.yaml</h3></div></div>
                <p className="muted small">
                  Several services, kept in a file. The token lives in a file of its own that only you can read.
                  Start it with <code>burrow connect --config burrow.yaml</code>.
                </p>
                <pre className="cmd-block"><code>{yamlExample(endpoint)}</code></pre>
              </section>

              <p className="muted small">
                Need a token for a machine without a browser? Create one under{" "}
                <Link className="link-inline" to="/clients?tab=tokens">Clients → Tokens</Link>.
              </p>
            </>
          )}
        </div>
      </section>
    </div>
  );
}
