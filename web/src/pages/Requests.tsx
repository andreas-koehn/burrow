import { useEffect, useMemo } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { parseTimeRange, timeRangeMs, widerRange, type TimeRange } from "@/lib/time-range";
import { withAIConfigDefaults } from "@/lib/aiConfig";
import { Badge, Button, EmptyState, PageHeader, SkeletonRows } from "@/components/ds";
import { LogView, type LogColumn } from "@/components/LogView";
import type { AiProvider, InspectorEntry, Service, ServiceAIConfig } from "@/lib/contract";

/** The relay returns at most this many requests per call, newest first. */
const LIMIT = 100;

/** The list query: period and search are applied by the relay, before its limit. */
function listPath(serviceId: string, range: TimeRange, search: string): string {
  const p = new URLSearchParams({ limit: String(LIMIT) });
  const ms = timeRangeMs(range);
  if (ms !== null) p.set("since", new Date(Date.now() - ms).toISOString());
  if (search) p.set("q", search);
  return `/services/${serviceId}/inspector/requests?${p.toString()}`;
}

/**
 * The requests the relay captured, one HTTP service at a time (the relay keeps them per
 * service). A row opens the request inspector, where a request is read and replayed.
 */
export default function Requests() {
  const nav = useNavigate();
  const qc = useQueryClient();
  // Range, search and service live in the URL, so a link reproduces the view.
  const [params, setParams] = useSearchParams();
  const range = parseTimeRange(params.get("range"));
  const search = params.get("q") ?? "";

  function setParam(name: string, value: string) {
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      if (value) next.set(name, value); else next.delete(name);
      return next;
    }, { replace: true });
  }
  const setRange = (r: TimeRange) => setParam("range", r === "24h" ? "" : r);

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const httpServices = useMemo(
    () => (Array.isArray(services.data) ? services.data : []).filter((s) => s.type === "http"),
    [services.data],
  );
  // Providers name the Provider column and tell "nothing set up yet" from "nothing to inspect".
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const providerOf = useMemo(() => {
    const m = new Map<string, string>();
    for (const p of Array.isArray(providers.data) ? providers.data : []) m.set(p.service_id, p.name || p.slug);
    return m;
  }, [providers.data]);

  // Without ?service= the list starts on the first http service.
  const serviceId = params.get("service") ?? httpServices[0]?.id ?? "";
  const serviceName = httpServices.find((s) => s.id === serviceId)?.name || serviceId;

  const cfg = useQuery({
    queryKey: ["service", serviceId, "ai-config"],
    queryFn: () =>
      apiFetch<Partial<ServiceAIConfig>>(`/services/${serviceId}/ai-config`).then(withAIConfigDefaults),
    retry: false,
    enabled: Boolean(serviceId),
  });
  // Under the inspector's own key, so everything that refreshes ["inspector", id] refreshes this too.
  const list = useQuery({
    queryKey: ["inspector", serviceId, "list", range, search],
    queryFn: () => apiFetch<InspectorEntry[]>(listPath(serviceId, range, search)),
    retry: false,
    enabled: Boolean(serviceId),
  });

  // New requests arrive over the event stream; the list follows them.
  useEffect(() => {
    if (typeof EventSource === "undefined" || !serviceId) return;
    const es = new EventSource("/api/v1/events");
    const onReq = () => qc.invalidateQueries({ queryKey: ["inspector", serviceId] });
    es.addEventListener("request", onReq);
    return () => { es.removeEventListener("request", onReq); es.close(); };
  }, [qc, serviceId]);

  const header = (
    <PageHeader title="Requests" subtitle="Requests the relay captured. Open one to inspect and replay it." />
  );

  // The page is always there; with nothing to list it says what is missing.
  if (services.data && httpServices.length === 0 && !params.get("service")) {
    if (providers.isLoading) {
      return <div className="inspector-page">{header}<SkeletonRows n={4} /></div>;
    }
    // Known to be empty; a provider list that failed to load is not "no providers".
    const noProviders = Array.isArray(providers.data) && providers.data.length === 0;
    return (
      <div className="inspector-page">
        {header}
        {noProviders ? (
          <EmptyState
            title="No providers yet"
            action={<Link className="btn btn-primary btn-sm" to="/gateway/providers">Add a provider</Link>}
          >
            Requests appear here once a provider is set up and receives traffic.
          </EmptyState>
        ) : (
          <EmptyState
            title="No HTTP services to inspect"
            action={<Button variant="primary" size="sm" onClick={() => nav("/clients/connect")}>Connect a client</Button>}
          >
            Connect a client with an HTTP service to see its requests here.
          </EmptyState>
        )}
      </div>
    );
  }

  // Model, tokens, cost and latency are left out: a captured request does not carry them yet.
  const columns: LogColumn<InspectorEntry>[] = [
    { id: "time", header: "Time", cell: (r) => <span className="mono small" title={r.ts}>{formatTimestamp(r.ts)}</span> },
    { id: "provider", header: "Provider", cell: (r) => providerOf.get(r.service_id) ?? "—" },
    { id: "method", header: "Method", cell: (r) => <span className="mono">{r.method}</span> },
    { id: "path", header: "Path", cell: (r) => <Link className="mono" to={`/gateway/requests/${r.service_id}/${r.id}`}>{r.path}</Link> },
    { id: "status", header: "Status", cell: (r) => <Badge nodot kind={`status-${Math.floor(r.status / 100)}xx`}>{r.status}</Badge> },
    { id: "cache", header: "Cache", cell: (r) => <span className="mono">{r.cache}</span> },
    {
      id: "guardrail",
      header: "Guardrail",
      cell: (r) => {
        const n = (r.redactions ?? []).reduce((sum, x) => sum + x.count, 0);
        return n > 0 ? `${n} redacted` : "—";
      },
    },
  ];

  const wider = widerRange(range);
  let empty;
  if (cfg.data && !cfg.data.inspector.enabled) {
    empty = (
      <EmptyState
        title={`Request inspector is off for ${serviceName}`}
        action={<Link className="btn btn-secondary btn-sm" to={`/services/${encodeURIComponent(serviceId)}`}>Open the service</Link>}
      >
        Request inspector is off for this service — enable in Access settings.
      </EmptyState>
    );
  } else if (search) {
    empty = <EmptyState title="No requests match your filter" />;
  } else if (wider) {
    empty = (
      <EmptyState
        title="No requests in this period"
        action={<Button variant="primary" size="sm" onClick={() => setRange(wider.value)}>{wider.label}</Button>}
      />
    );
  } else {
    empty = (
      <EmptyState title="No requests yet">
        Requests appear here as soon as traffic reaches this service.
      </EmptyState>
    );
  }

  const failed = services.isError && !services.data ? services : list;

  return (
    <div className="inspector-page">
      {header}
      <LogView<InspectorEntry>
        label="Requests"
        rows={services.data ? list.data : undefined}
        rowKey={(r) => r.id}
        columns={columns}
        isLoading={services.isLoading || list.isLoading}
        error={failed.error}
        onRetry={() => void failed.refetch()}
        range={range}
        onRangeChange={setRange}
        search={search}
        onSearchChange={(s) => setParam("q", s)}
        filters={
          <label className="filter-label">
            <span>Service</span>
            <select
              aria-label="Service"
              value={serviceId}
              onChange={(e) => setParam("service", e.target.value)}
              className="input"
            >
              {/* A linked-to service stays selectable while the list loads or when it is not an http service. */}
              {serviceId && !httpServices.some((s) => s.id === serviceId) && <option value={serviceId}>{serviceId}</option>}
              {httpServices.map((s) => <option key={s.id} value={s.id}>{s.name || s.id}</option>)}
            </select>
          </label>
        }
        empty={empty}
      />
      {(list.data?.length ?? 0) >= LIMIT && (
        <p className="muted small">
          Showing the newest {LIMIT} requests that match. Use the filter to find older ones.
        </p>
      )}
    </div>
  );
}
