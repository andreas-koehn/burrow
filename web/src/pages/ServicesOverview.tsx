import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { PageHeader, MetricStrip, MetricTile, ErrorNotice, Button, SkeletonRows } from "@/components/ds";
import { SetupChecklist, type ChecklistStep } from "@/components/SetupChecklist";
import { InstallLines } from "@/components/InstallLines";
import { RelativeTime } from "@/components/RelativeTime";
import { StatusBadge } from "@/components/StatusBadge";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { GLOSSARY } from "@/lib/glossary";
import { NAVIGATIONS } from "@/lib/navigation";
import { serviceState } from "@/lib/status";
import { clientsOnline, useKnownClients } from "@/lib/useKnownClients";
import { useRelayNotices } from "@/lib/useRelayNotices";
import type { ConnectionLog, Service } from "@/lib/contract";

// The three lines of the Connect page, split over the first two steps.
const SIGN_IN_LINES = ["install", "login"] as const;
const RUN_LINE = ["run"] as const;

/** As many services as the Overview lists; the Services page has them all. */
const LISTED = 8;

/** Online first, then whoever was served last; a service no client ever served comes last. */
function byLastServed(a: Service, b: Service): number {
  const online = Number(serviceState(b) === "online") - Number(serviceState(a) === "online");
  if (online !== 0) return online;
  return (b.last_seen ?? "").localeCompare(a.last_seen ?? "");
}

/** A client token as GET /tokens lists it. */
interface Token { id: string; name: string; last_used: string | null; created_at: string; }

export default function ServicesOverview() {
  const { loading: authLoading } = useAuth();
  // GET /clients/known: tunnels:read:any. GET /connection-logs: audit:read. POST /services: services:configure:any.
  const canListClients = useCan(P.tunnelsReadAny);
  const canReadTraffic = useCan(P.auditRead);
  const canCreateService = useCan(P.servicesConfigureAny);
  const notices = useRelayNotices();

  // The rows of the Clients page, from the request the sidebar makes too.
  // Needs tunnels:read:any; everyone else gets a dash instead of a count.
  const clients = useKnownClients({ enabled: canListClients });

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });

  // Same key and fetch as the Tokens tab of Clients.
  const tokens = useQuery({
    queryKey: ["tokens"],
    queryFn: () => apiFetch<Token[]>("/tokens"),
    retry: false,
    staleTime: 30_000,
  });

  // One row is enough to know whether anything ever came through. Needs audit:read.
  const anyTraffic = useQuery({
    queryKey: ["connection-logs-any"],
    queryFn: () => apiFetch<ConnectionLog[]>("/connection-logs?limit=1"),
    enabled: canReadTraffic,
    retry: false,
    staleTime: 30_000,
  });

  const svc = Array.isArray(services.data) ? services.data : [];
  const known = clients.rows ?? [];
  // The live sessions of the clients that are online.
  const cls = known.flatMap((c) => c.sessions);
  const toks = Array.isArray(tokens.data) ? tokens.data : [];
  const liveNow = svc.filter((s) => s.connected).length;
  // As on the Services page: a service that is reachable through the AI gateway only is a provider's business.
  const own = svc.filter((s) => !s.gateway_only);
  const gatewayOnly = svc.length - own.length;
  const listed = [...own].sort(byLastServed).slice(0, LISTED);

  const trafficValue = canListClients && clients.rows
    ? `${formatBytes(cls.reduce((a, c) => a + c.total_bytes_in, 0))} / ${formatBytes(cls.reduce((a, c) => a + c.total_bytes_out, 0))}`
    : "—";

  // The checklist states facts, so it waits until every answer it needs is in
  // and stays away when one of them failed.
  const checklistReady = !authLoading && services.isSuccess && tokens.isSuccess
    && (!canListClients || clients.isSuccess) && (!canReadTraffic || anyTraffic.isSuccess);
  // The relay serves this dashboard, so its address is where the page came from.
  const relayOrigin = typeof window !== "undefined" ? window.location.origin : "";
  const steps: ChecklistStep[] = [
    {
      id: "token",
      title: "Install and sign in",
      description: "Install burrow on your machine and sign it in to this relay.",
      // GET /tokens lists only the caller's own tokens. A client, a live or a
      // saved service proves that somebody's token did the job.
      done: toks.length > 0 || cls.length > 0 || liveNow > 0 || svc.length > 0,
      content: <InstallLines relayOrigin={relayOrigin} lines={SIGN_IN_LINES} />,
    },
    {
      id: "client",
      title: "Connect a client",
      description: "Run burrow on your machine; that registers it as a client of this relay.",
      // A used token or a live service is proof enough for someone who cannot list clients.
      done: cls.length > 0 || toks.some((t) => t.last_used) || liveNow > 0,
      content: <InstallLines relayOrigin={relayOrigin} lines={RUN_LINE} />,
      action: { label: "Connect a client", to: "/clients/connect" },
    },
    {
      id: "service",
      title: "Expose a service",
      description: "A service is the saved address and access mode (open, API key or Burrow login) of a local port; it stays when no client is connected.",
      done: svc.length > 0,
      ...(canCreateService ? { action: { label: "New service", to: "/services?new=1" } } : {}),
    },
    // The traffic log needs audit:read, so only whoever holds it gets this step.
    ...(canReadTraffic ? [{
      id: "traffic",
      title: "Receive the first request",
      description: "A live connection to a service is a tunnel; it exists while the client is connected and forwards traffic.",
      done: Array.isArray(anyTraffic.data) && anyTraffic.data.length > 0,
      action: { label: "Open traffic", to: "/traffic" },
    }] : []),
  ];

  const quickActions = (
    <div className="home-quick-actions">
      <Link className="btn btn-primary btn-sm" to="/clients/connect">Connect a client</Link>
      {/* ?new=1 opens the dialog the label promises. */}
      {canCreateService && <Link className="btn btn-secondary btn-sm" to="/services?new=1">New service</Link>}
    </div>
  );

  const stripLoading = authLoading || services.isLoading || (canListClients && clients.isLoading);

  return (
    <div className="home-page">
      {/* Above the title: a notice is about the relay, not about one of the page's parts. */}
      {notices.length > 0 && (
        <div className="alerts-strip">
          {notices.map((n) => (
            <ErrorNotice key={n.id} variant="warn" role="status" action={<Link to={n.action.to}>{n.action.label} →</Link>}>
              {n.message}
            </ErrorNotice>
          ))}
        </div>
      )}

      <PageHeader title="Overview" subtitle="Machines, services and traffic on this relay." actions={quickActions} />
      {/* A pattern, not an address anybody can open: muted, and without a copy button. */}
      <p className="address-pattern">
        <span className="muted small">Services are published under</span>{" "}
        <span className="mono muted">{relayOrigin}{NAVIGATIONS.services.namespace}</span>
      </p>

      {services.error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void services.refetch()}>Retry</Button>}
        >
          Couldn't load services:{" "}
          {services.error instanceof ApiError ? services.error.message : "Unknown error"}
        </ErrorNotice>
      ) : stripLoading ? (
        <SkeletonRows n={3} />
      ) : (
        <MetricStrip ariaLabel="Overview">
          <MetricTile
            label="Clients online"
            value={canListClients && clients.rows ? String(clientsOnline(known)) : "—"}
            sub="machines connected"
            tooltip={GLOSSARY.client.replace(/`/g, "")}
            to="/clients"
          />
          <MetricTile
            label="Services"
            value={String(svc.length)}
            sub="saved configurations"
            tooltip={GLOSSARY.service}
            to="/services"
          />
          <MetricTile
            label="Live now"
            value={String(liveNow)}
            sub="forwarding right now"
            tooltip={GLOSSARY.tunnel}
            to="/services?live=1"
          />
          {/* Session totals of the clients connected right now; the relay has no 24-hour figure. */}
          <MetricTile label="Traffic" value={trafficValue} sub="in / out, connected clients" to="/traffic" />
        </MetricStrip>
      )}

      {checklistReady && <SetupChecklist title="Set up Services" steps={steps} />}

      {svc.length > 0 && (
        <div className="home-services">
          {listed.length > 0 && (
            <div className="table-wrap">
              <table className="data" aria-label="Services">
                <thead><tr><th>Name</th><th>Status</th><th>Last served</th></tr></thead>
                <tbody>
                  {listed.map((s) => {
                    const state = serviceState(s);
                    return (
                      <tr key={s.id}>
                        <td className="col-name"><Link to={`/services/${s.id}`}>{s.name || s.id}</Link></td>
                        <td><StatusBadge state={state} /></td>
                        <td>{state === "online" ? "now" : <RelativeTime value={s.last_seen} />}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
          <p className="home-services-more small">
            <Link className="link-inline" to="/services">All services</Link>
            {gatewayOnly > 0 && (
              <>
                {" · "}
                <Link className="link-inline" to="/gateway/providers">
                  {gatewayOnly}{listed.length > 0 ? " more" : ""} {gatewayOnly === 1 ? "belongs" : "belong"} to the AI gateway
                </Link>
              </>
            )}
          </p>
        </div>
      )}
    </div>
  );
}
