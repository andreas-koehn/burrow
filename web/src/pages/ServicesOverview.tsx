import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { PageHeader, MetricStrip, MetricTile, ErrorNotice, Button, SkeletonRows } from "@/components/ds";
import { SetupChecklist, type ChecklistStep } from "@/components/SetupChecklist";
import { InstallLines } from "@/components/InstallLines";
import { useAuth } from "@/auth/useAuth";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { GLOSSARY } from "@/lib/glossary";
import { useRelayNotices } from "@/lib/useRelayNotices";
import type { ClientView, ConnectionLog, Service } from "@/lib/contract";

// The three lines of the Connect page, split over the first two steps.
const SIGN_IN_LINES = ["install", "login"] as const;
const RUN_LINE = ["run"] as const;

/** A client token as GET /tokens lists it. */
interface Token { id: string; name: string; last_used: string | null; created_at: string; }

export default function ServicesOverview() {
  const { user, loading: authLoading } = useAuth();
  const isAdmin = user?.role === "admin";
  const notices = useRelayNotices();

  // Admin-only endpoint; everyone else gets a dash instead of a count.
  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    enabled: isAdmin,
    retry: false,
  });

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

  // One row is enough to know whether anything ever came through. Admin-only endpoint.
  const anyTraffic = useQuery({
    queryKey: ["connection-logs-any"],
    queryFn: () => apiFetch<ConnectionLog[]>("/connection-logs?limit=1"),
    enabled: isAdmin,
    retry: false,
    staleTime: 30_000,
  });

  const svc = Array.isArray(services.data) ? services.data : [];
  const cls = Array.isArray(clients.data) ? clients.data : [];
  const toks = Array.isArray(tokens.data) ? tokens.data : [];
  const liveNow = svc.filter((s) => s.connected).length;

  const trafficValue = isAdmin && clients.data
    ? `${formatBytes(cls.reduce((a, c) => a + c.total_bytes_in, 0))} / ${formatBytes(cls.reduce((a, c) => a + c.total_bytes_out, 0))}`
    : "—";

  // The checklist states facts, so it waits until every answer it needs is in
  // and stays away when one of them failed.
  const checklistReady = !authLoading && services.isSuccess && tokens.isSuccess
    && (!isAdmin || (clients.isSuccess && anyTraffic.isSuccess));
  // The relay serves this dashboard, so its address is where the page came from.
  const relayOrigin = typeof window !== "undefined" ? window.location.origin : "";
  const steps: ChecklistStep[] = [
    {
      id: "token",
      title: "Install and sign in",
      description: "Install burrow on your machine and sign it in to this relay with a client token.",
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
      action: { label: "New service", to: "/services?new=1" },
    },
    // Only an admin can read the traffic log, so only an admin gets this step.
    ...(isAdmin ? [{
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
      <Link className="btn btn-secondary btn-sm" to="/services?new=1">New service</Link>
    </div>
  );

  const stripLoading = authLoading || services.isLoading || (isAdmin && clients.isLoading);

  return (
    <div className="home-page">
      <PageHeader title="Overview" subtitle="Machines, services and traffic on this relay." actions={quickActions} />

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
            value={isAdmin && clients.data ? String(cls.length) : "—"}
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

      {notices.length > 0 && (
        <div className="alerts-strip">
          {notices.map((n) => (
            <ErrorNotice key={n.id} variant="warn" role="status" action={<Link to={n.action.to}>{n.action.label} →</Link>}>
              {n.message}
            </ErrorNotice>
          ))}
        </div>
      )}

      {checklistReady && <SetupChecklist title="Set up Services" steps={steps} />}
    </div>
  );
}
