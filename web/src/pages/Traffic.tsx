import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { apiFetch, downloadFile } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { statusLabel } from "@/lib/status";
import { parseTimeRange, timeRangeMs, widerRange, type TimeRange } from "@/lib/time-range";
import { Button, Badge, EmptyState, PageHeader } from "@/components/ds";
import { LogView, type LogColumn } from "@/components/LogView";
import type { ConnectionLog, ConnectionLogRollup, ConnectionLogKind, ConnectionLogStatus, Service } from "@/lib/contract";

const PAGE_SIZE = 50;

// Format bytes as human-readable string.
function fmtBytes(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(n);
}

// Kind badge kind mapping (using Badge `kind` prop for CSS).
function kindClass(kind: ConnectionLogKind): string {
  switch (kind) {
    case "http_proxy": return "info";
    case "tcp_proxy": return "warning";
    case "control": return "default";
  }
}

const KIND_LABELS: Record<ConnectionLogKind, string> = {
  http_proxy: "HTTP proxy",
  tcp_proxy: "TCP proxy",
  control: "Control",
};

// Status badge kind mapping — maps onto the existing status-* CSS variants.
function statusClass(status: ConnectionLogStatus): string {
  switch (status) {
    case "closed_clean": return "status-connected";
    case "closed_idle":  return "status-idle";
    case "closed_error": return "status-suspended";
    case "rejected":     return "status-suspended";
  }
}

/** The filters every connection-log endpoint understands; the window ends at the time of the call. */
function logParams(f: { kind: string; service: string; range: TimeRange; q: string }): URLSearchParams {
  const now = Date.now();
  const p = new URLSearchParams();
  if (f.kind) p.set("kind", f.kind);
  if (f.service) p.set("service_id", f.service);
  // "All" sends no bounds, so the list and the export reach as far back as retention keeps logs.
  const ms = timeRangeMs(f.range);
  if (ms !== null) {
    p.set("since", new Date(now - ms).toISOString());
    p.set("until", new Date(now).toISOString());
  }
  if (f.q) p.set("q", f.q);
  return p;
}

function LogDetail({ row, service }: { row: ConnectionLog; service: string }) {
  const facts: [string, string][] = [
    ["Service", service],
    ["Started", row.started_at],
    ["Ended", row.ended_at],
    ["Reason", row.reason],
    ["User agent", row.user_agent],
    ["Client session", row.client_session_id],
    ["Tunnel", row.tunnel_id],
    ["User", row.user_id],
    ["Bytes in", String(row.bytes_in)],
    ["Bytes out", String(row.bytes_out)],
  ];
  return (
    <dl className="def-list">
      {facts.map(([key, value]) => (
        <div className="def-row" key={key}>
          <dt className="def-key">{key}</dt>
          <dd className="def-val">{value || "—"}</dd>
        </div>
      ))}
    </dl>
  );
}

export default function Traffic() {
  // Range, search and service live in the URL, so a link reproduces the view.
  const [params, setParams] = useSearchParams();
  const range = parseTimeRange(params.get("range"));
  const searchQ = params.get("q") ?? "";
  const serviceFilter = params.get("service") ?? "";
  const [kindFilter, setKindFilter] = useState<ConnectionLogKind | "">("");
  const [rollups, setRollups] = useState(false);

  function setParam(name: string, value: string) {
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      if (value) next.set(name, value); else next.delete(name);
      return next;
    }, { replace: true });
  }
  const setRange = (r: TimeRange) => setParam("range", r === "24h" ? "" : r);

  const filterParams = (withSearch: boolean) =>
    logParams({ kind: kindFilter, service: serviceFilter, range, q: withSearch ? searchQ : "" });

  // Logs, a page at a time; the cursor is the id of the last row shown.
  const logsQuery = useInfiniteQuery({
    queryKey: ["connection-logs", kindFilter, serviceFilter, range, searchQ],
    queryFn: ({ pageParam }) => {
      const p = filterParams(true);
      p.set("limit", String(PAGE_SIZE));
      if (pageParam) p.set("before_id", pageParam);
      return apiFetch<ConnectionLog[]>(`/connection-logs?${p.toString()}`);
    },
    initialPageParam: "",
    getNextPageParam: (last) => (last.length < PAGE_SIZE ? undefined : last.at(-1)?.id),
    enabled: !rollups,
    retry: false,
  });

  const rollupsQuery = useQuery({
    queryKey: ["connection-logs-rollups", serviceFilter, kindFilter, range],
    queryFn: () => apiFetch<ConnectionLogRollup[]>(`/connection-logs/rollups?${filterParams(false).toString()}`),
    enabled: rollups,
    retry: false,
  });

  // Services for the service picker and the Service column.
  const servicesQuery = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const services = useMemo(() => (Array.isArray(servicesQuery.data) ? servicesQuery.data : []), [servicesQuery.data]);
  const serviceName = useMemo(() => {
    const m = new Map<string, string>();
    for (const s of services) m.set(s.id, s.name);
    return m;
  }, [services]);
  const serviceCell = (id: string) => serviceName.get(id) ?? <span className="mono">{id}</span>;

  function handleExport() {
    const p = filterParams(true);
    p.set("format", "ndjson");
    void downloadFile(`/connection-logs/export?${p.toString()}`, "connection-logs.ndjson");
  }

  const logs = useMemo(() => logsQuery.data?.pages.flat(), [logsQuery.data]);
  // The rollups endpoint has no text search, so the Filter box narrows what it returned.
  const needle = searchQ.toLowerCase();
  const rollupRows = rollupsQuery.data?.filter((r) =>
    `${r.day} ${r.kind} ${KIND_LABELS[r.kind] ?? ""} ${r.service_id} ${serviceName.get(r.service_id) ?? ""}`.toLowerCase().includes(needle));

  const logColumns: LogColumn<ConnectionLog>[] = [
    { id: "time", header: "Time", cell: (r) => <span className="mono small" title={r.started_at}>{formatTimestamp(r.started_at)}</span> },
    { id: "service", header: "Service", cell: (r) => serviceCell(r.service_id) },
    { id: "ip", header: "Client IP", cell: (r) => <span className="mono small">{r.source_ip}</span> },
    { id: "kind", header: "Protocol", cell: (r) => <span data-kind={r.kind}><Badge kind={kindClass(r.kind)}>{KIND_LABELS[r.kind]}</Badge></span> },
    { id: "status", header: "Status", cell: (r) => <span data-status={r.status}><Badge kind={statusClass(r.status)}>{statusLabel(r.status)}</Badge></span> },
    { id: "duration", header: "Duration", numeric: true, cell: (r) => <span className="mono small">{r.duration_ms}ms</span> },
    { id: "bytes", header: "Bytes", numeric: true, cell: (r) => <span className="mono small">{fmtBytes(r.bytes_in)} in / {fmtBytes(r.bytes_out)} out</span> },
  ];

  const mono = (v: string | number) => <span className="mono small">{v}</span>;
  const rollupColumns: LogColumn<ConnectionLogRollup>[] = [
    { id: "day", header: "Day", cell: (r) => mono(r.day) },
    { id: "kind", header: "Protocol", cell: (r) => <Badge kind={kindClass(r.kind)}>{KIND_LABELS[r.kind]}</Badge> },
    { id: "service", header: "Service", cell: (r) => serviceCell(r.service_id) },
    { id: "sessions", header: "Sessions", numeric: true, cell: (r) => mono(r.sessions) },
    { id: "in", header: "Bytes in", numeric: true, cell: (r) => mono(fmtBytes(r.bytes_in)) },
    { id: "out", header: "Bytes out", numeric: true, cell: (r) => mono(fmtBytes(r.bytes_out)) },
    { id: "avg", header: "Avg ms", numeric: true, cell: (r) => mono(r.avg_duration_ms) },
    { id: "p95", header: "P95 ms", numeric: true, cell: (r) => mono(r.p95_duration_ms) },
  ];
  // v0.5.1 Q12: the API omits top_source_ips when the operator turned the setting off;
  // the column shows only when at least one row carries the field.
  if (rollupRows?.some((r) => r.top_source_ips !== undefined)) {
    rollupColumns.push({
      id: "top-ips",
      header: "Top source IPs",
      cell: (r) => (
        <span className="mono small" data-testid="top-source-ips">
          {!r.top_source_ips || r.top_source_ips.length === 0
            ? "—"
            : r.top_source_ips.map((t) => `${t.ip} (${t.sessions})`).join(", ")}
        </span>
      ),
    });
  }

  const filters = (
    <>
      <label className="filter-label">
        <span>Protocol</span>
        <select
          aria-label="Protocol"
          value={kindFilter}
          onChange={(e) => setKindFilter(e.target.value as ConnectionLogKind | "")}
          className="input"
        >
          <option value="">All</option>
          <option value="control">Control</option>
          <option value="http_proxy">HTTP proxy</option>
          <option value="tcp_proxy">TCP proxy</option>
        </select>
      </label>
      <label className="filter-label">
        <span>Service</span>
        <select
          aria-label="Service"
          value={serviceFilter}
          onChange={(e) => setParam("service", e.target.value)}
          className="input"
        >
          <option value="">All</option>
          {/* A linked-to service stays selectable while the list loads or after it was removed. */}
          {serviceFilter && !serviceName.has(serviceFilter) && <option value={serviceFilter}>{serviceFilter}</option>}
          {services.map((s) => <option key={s.id} value={s.id}>{s.name || s.id}</option>)}
        </select>
      </label>
      <label className="checkbox-row small">
        <input type="checkbox" aria-label="Rollups" checked={rollups} onChange={(e) => setRollups(e.target.checked)} />
        <span>Rollups</span>
      </label>
    </>
  );

  const wider = widerRange(range);
  const what = rollups ? "rollups" : "traffic";
  const empty = (
    <EmptyState
      title={wider ? `No ${what} in this period` : `No ${what} yet`}
      action={wider
        ? <Button variant="primary" size="sm" onClick={() => setRange(wider.value)}>{wider.label}</Button>
        : undefined}
    >
      Connections are recorded on session close.
    </EmptyState>
  );

  const shared = {
    label: "Traffic",
    range,
    onRangeChange: setRange,
    search: searchQ,
    onSearchChange: (s: string) => setParam("q", s),
    filters,
    empty,
  };

  return (
    <div className="traffic-page">
      <PageHeader
        title="Traffic"
        subtitle="Connections to your services, recorded when each one closes."
        actions={<Button variant="secondary" size="sm" onClick={handleExport}>Export</Button>}
      />
      {rollups ? (
        <LogView<ConnectionLogRollup>
          {...shared}
          rows={rollupRows}
          rowKey={(r) => `${r.day}-${r.service_id}-${r.kind}`}
          columns={rollupColumns}
          isLoading={rollupsQuery.isLoading}
          error={rollupsQuery.error}
          onRetry={() => void rollupsQuery.refetch()}
        />
      ) : (
        <LogView<ConnectionLog>
          {...shared}
          rows={logs}
          rowKey={(r) => r.id}
          columns={logColumns}
          isLoading={logsQuery.isLoading}
          error={logsQuery.error}
          onRetry={() => void logsQuery.refetch()}
          detail={(r) => <LogDetail row={r} service={serviceName.get(r.service_id) ?? r.service_id} />}
          hasMore={logsQuery.hasNextPage}
          onLoadMore={() => void logsQuery.fetchNextPage()}
        />
      )}
    </div>
  );
}
