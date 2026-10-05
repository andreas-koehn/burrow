import { useEffect, useMemo } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { parseTimeRange, timeRangeMs, type TimeRange } from "@/lib/time-range";
import { withAIConfigDefaults } from "@/lib/aiConfig";
import { Badge, Button, EmptyState, PageHeader } from "@/components/ds";
import { LogView, type LogColumn } from "@/components/LogView";
import type { InspectorEntry, Service, ServiceAIConfig } from "@/lib/contract";

/**
 * The requests the relay captured, one HTTP service at a time (the relay keeps them per
 * service). A row opens the request inspector, where a request is read and replayed.
 */
export default function Requests() {
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
  const list = useQuery({
    queryKey: ["inspector", serviceId],
    queryFn: () => apiFetch<InspectorEntry[]>(`/services/${serviceId}/inspector/requests?limit=100`),
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

  // The relay returns the newest requests it kept; period and search narrow them here.
  const rows = useMemo(() => {
    if (!list.data) return undefined;
    // The period ends when the list was last fetched; the event stream keeps that current.
    const since = list.dataUpdatedAt - timeRangeMs(range);
    const q = search.toLowerCase();
    return list.data.filter((r) =>
      Date.parse(r.ts) >= since && `${r.path} ${r.method} ${r.req_body}`.toLowerCase().includes(q));
  }, [list.data, list.dataUpdatedAt, range, search]);

  const header = (
    <PageHeader title="Requests" subtitle="Requests the relay captured. Open one to inspect and replay it." />
  );

  // The page is always there; without a service behind a provider it says what is missing.
  if (services.data && httpServices.length === 0 && !params.get("service")) {
    return (
      <div className="inspector-page">
        {header}
        <EmptyState
          title="No providers yet"
          action={<Link className="btn btn-primary btn-sm" to="/gateway/providers">Add a provider</Link>}
        >
          Requests appear here once a provider is set up and receives traffic.
        </EmptyState>
      </div>
    );
  }

  // Columns the relay has no data for yet (provider, model, tokens, cost, latency) are left out.
  const columns: LogColumn<InspectorEntry>[] = [
    { id: "time", header: "Time", cell: (r) => <span className="mono small" title={r.ts}>{formatTimestamp(r.ts)}</span> },
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

  let empty;
  if (cfg.data && !cfg.data.inspector.enabled) {
    empty = (
      <EmptyState title={`Request capture is off for ${serviceName}`}>
        Requests to this service are not being recorded.
      </EmptyState>
    );
  } else if (list.data?.length === 0) {
    empty = (
      <EmptyState title="No requests yet">
        Requests appear here as soon as traffic reaches this service.
      </EmptyState>
    );
  } else if (search) {
    empty = <EmptyState title="No requests match your filter" />;
  } else {
    empty = (
      <EmptyState
        title="No requests in this period"
        action={range !== "7d"
          ? <Button variant="primary" size="sm" onClick={() => setRange("7d")}>Show the last 7 days</Button>
          : undefined}
      />
    );
  }

  const failed = services.isError && !services.data ? services : list;

  return (
    <div className="inspector-page">
      {header}
      <LogView<InspectorEntry>
        label="Requests"
        rows={services.data ? rows : undefined}
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
    </div>
  );
}
