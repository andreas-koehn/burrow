import { useState, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { apiFetch, downloadFile } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { statusLabel } from "@/lib/status";
import { Button, Badge, EmptyState, PageHeader, SkeletonRows } from "@/components/ds";
import type { ConnectionLog, ConnectionLogRollup, ConnectionLogKind, ConnectionLogStatus, Service } from "@/lib/contract";

// Translate date-range preset to since/until ISO strings.
function presetRange(preset: string): { since: string; until: string } {
  const now = new Date();
  const until = now.toISOString();
  switch (preset) {
    case "1h":
      return { since: new Date(now.getTime() - 3600_000).toISOString(), until };
    case "24h":
      return { since: new Date(now.getTime() - 86_400_000).toISOString(), until };
    case "7d":
      return { since: new Date(now.getTime() - 7 * 86_400_000).toISOString(), until };
    case "30d":
      return { since: new Date(now.getTime() - 30 * 86_400_000).toISOString(), until };
    default:
      return { since: "", until: "" };
  }
}

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

const BACK = { to: "/settings", label: "Settings" } as const;

export default function ConnectionLogs() {
  const [kindFilter, setKindFilter] = useState<ConnectionLogKind | "">("");
  const [serviceFilter, setServiceFilter] = useState("");
  const [datePreset, setDatePreset] = useState("24h");
  const [searchQ, setSearchQ] = useState("");
  const [rollups, setRollups] = useState(false);
  const [allLogs, setAllLogs] = useState<ConnectionLog[]>([]);

  const { since, until } = presetRange(datePreset);

  // Build query params for the logs endpoint.
  function buildLogParams(cursor: string | null) {
    const p = new URLSearchParams();
    if (kindFilter) p.set("kind", kindFilter);
    if (serviceFilter) p.set("service_id", serviceFilter);
    if (since) p.set("since", since);
    if (until) p.set("until", until);
    if (searchQ) p.set("q", searchQ);
    p.set("limit", "50");
    if (cursor) p.set("before_id", cursor);
    return p.toString();
  }

  // Logs query — resets pagination when filters change.
  const logsQuery = useQuery({
    queryKey: ["connection-logs", kindFilter, serviceFilter, datePreset, searchQ],
    queryFn: async () => {
      const rows = await apiFetch<ConnectionLog[]>(`/connection-logs?${buildLogParams(null)}`);
      setAllLogs(rows);
      return rows;
    },
    enabled: !rollups,
    retry: false,
  });

  // Rollups query.
  const rollupsQuery = useQuery({
    queryKey: ["connection-logs-rollups", serviceFilter, kindFilter, datePreset],
    queryFn: () => {
      const p = new URLSearchParams();
      if (serviceFilter) p.set("service_id", serviceFilter);
      if (kindFilter) p.set("kind", kindFilter);
      if (since) p.set("since", since);
      if (until) p.set("until", until);
      return apiFetch<ConnectionLogRollup[]>(`/connection-logs/rollups?${p.toString()}`);
    },
    enabled: rollups,
    retry: false,
  });

  // Services for the service combobox.
  const servicesQuery = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });

  // Map service_id → Service for fast lookup in the Service column (B4).
  const serviceMap = useMemo(() => {
    const m = new Map<string, Service>();
    for (const s of servicesQuery.data ?? []) m.set(s.id, s);
    return m;
  }, [servicesQuery.data]);

  // Load more (cursor pagination).
  async function loadMore() {
    const last = allLogs.at(-1);
    if (!last) return;
    const rows = await apiFetch<ConnectionLog[]>(
      `/connection-logs?${buildLogParams(last.id)}`,
    );
    setAllLogs((prev) => [...prev, ...rows]);
  }

  // Export handler.
  function handleExport() {
    const p = new URLSearchParams({ format: "ndjson" });
    if (kindFilter) p.set("kind", kindFilter);
    if (serviceFilter) p.set("service_id", serviceFilter);
    if (since) p.set("since", since);
    if (until) p.set("until", until);
    if (searchQ) p.set("q", searchQ);
    void downloadFile(`/connection-logs/export?${p.toString()}`, "connection-logs.ndjson");
  }

  const logs = allLogs;
  const rollupRows = rollupsQuery.data ?? [];
  const loading = !rollups
    ? !logsQuery.data && logsQuery.isLoading
    : !rollupsQuery.data && rollupsQuery.isLoading;

  return (
    <div className="connection-logs-page">
      <PageHeader
        back={BACK}
        title="Connection logs"
        actions={<Button variant="secondary" size="sm" onClick={handleExport}>Export</Button>}
      />

      {/* Filter toolbar — row 1: Kind / Service / Range selects + search */}
      <div className="filter-row">
        {/* Kind filter — native <select> for testability */}
        <label className="filter-label">
          <span>Kind</span>
          <select
            aria-label="Kind"
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

        {/* Service filter */}
        <label className="filter-label">
          <span>Service</span>
          <select
            aria-label="Service"
            value={serviceFilter}
            onChange={(e) => setServiceFilter(e.target.value)}
            className="input"
          >
            <option value="">All</option>
            {(servicesQuery.data ?? []).map((s) => (
              <option key={s.id} value={s.id}>{s.name}</option>
            ))}
          </select>
        </label>

        {/* Date range preset */}
        <label className="filter-label">
          <span>Range</span>
          <select
            aria-label="Date range"
            value={datePreset}
            onChange={(e) => setDatePreset(e.target.value)}
            className="input"
          >
            <option value="1h">Last hour</option>
            <option value="24h">Last 24h</option>
            <option value="7d">Last 7d</option>
            <option value="30d">Last 30d</option>
            <option value="custom">Custom</option>
          </select>
        </label>

        {/* Free-text search */}
        <input
          type="search"
          aria-label="Search connection logs"
          placeholder="search IP / kind / service"
          value={searchQ}
          onChange={(e) => setSearchQ(e.target.value)}
          className="input flex-1"
        />
      </div>

      {/* Filter toolbar — row 2: Rollups checkbox on its own line */}
      <div className="filter-row">
        {/* Rollups toggle — native checkbox for testability */}
        <label className="checkbox-row small">
          <input
            type="checkbox"
            aria-label="Rollups"
            checked={rollups}
            onChange={(e) => setRollups(e.target.checked)}
          />
          <span>Rollups</span>
        </label>
      </div>

      {/* Table */}
      {loading ? (
        <SkeletonRows n={6} />
      ) : rollups ? (
        /* Rollups table */
        rollupRows.length === 0 ? (
          <EmptyState title="No rollups yet">Connections are recorded on session close.</EmptyState>
        ) : (
          <div className="table-wrap">
            <table className="data" aria-label="Connection logs">
              <thead>
                <tr>
                  <th>Day</th>
                  <th>Kind</th>
                  <th>Service</th>
                  <th>Sessions</th>
                  <th>Bytes in</th>
                  <th>Bytes out</th>
                  <th>Avg ms</th>
                  <th>P95 ms</th>
                  {/* v0.5.1 Q12: render the "Top source IPs" column header
                      only when at least one row in the page carries the
                      top_source_ips field. When the operator has the toggle
                      OFF, the API omits the field entirely → no header. */}
                  {rollupRows.some((r) => r.top_source_ips !== undefined) && (
                    <th>Top source IPs</th>
                  )}
                </tr>
              </thead>
              <tbody>
                {rollupRows.map((r, i) => (
                  <tr key={`${r.day}-${r.service_id}-${r.kind}-${i}`}>
                    <td className="mono small">{r.day}</td>
                    <td>
                      <Badge kind={kindClass(r.kind)}>{KIND_LABELS[r.kind]}</Badge>
                    </td>
                    <td className="small">{serviceMap.get(r.service_id)?.name ?? <span className="mono">{r.service_id}</span>}</td>
                    <td className="mono small">{r.sessions}</td>
                    <td className="mono small">{fmtBytes(r.bytes_in)}</td>
                    <td className="mono small">{fmtBytes(r.bytes_out)}</td>
                    <td className="mono small">{r.avg_duration_ms}</td>
                    <td className="mono small">{r.p95_duration_ms}</td>
                    {rollupRows.some((rr) => rr.top_source_ips !== undefined) && (
                      <td className="mono small" data-testid="top-source-ips">
                        {/* v0.5.2 BACKLOG #6: render "—" uniformly for both
                            undefined and empty []. Pre-fix the undefined
                            branch rendered "" (blank cell) while the empty
                            branch rendered "—" — visually inconsistent for
                            two semantically-equivalent empty states. */}
                        {!r.top_source_ips || r.top_source_ips.length === 0
                          ? "—"
                          : r.top_source_ips
                              .map((t) => `${t.ip} (${t.sessions})`)
                              .join(", ")}
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )
      ) : (
        /* Logs table */
        logs.length === 0 ? (
          <EmptyState title="No connection logs yet">Connections are recorded on session close.</EmptyState>
        ) : (
          <>
            <div className="table-wrap">
              <table className="data" aria-label="Connection logs">
                <thead>
                  <tr>
                    <th>When</th>
                    <th>Kind</th>
                    <th>Service</th>
                    <th>Source IP</th>
                    <th className="col-num">Duration</th>
                    <th className="col-num">Bytes in</th>
                    <th className="col-num">Bytes out</th>
                    <th>Status</th>
                    <th>Reason</th>
                  </tr>
                </thead>
                <tbody>
                  {logs.map((r) => (
                    <tr key={r.id}>
                      <td className="mono small" title={r.started_at}>{formatTimestamp(r.started_at)}</td>
                      <td data-kind={r.kind}>
                        <Badge kind={kindClass(r.kind)}>{KIND_LABELS[r.kind]}</Badge>
                      </td>
                      <td className="small">{serviceMap.get(r.service_id)?.name ?? <span className="mono">{r.service_id}</span>}</td>
                      <td className="mono small">{r.source_ip}</td>
                      <td className="mono small col-num">{r.duration_ms}ms</td>
                      <td className="mono small col-num">{fmtBytes(r.bytes_in)}</td>
                      <td className="mono small col-num">{fmtBytes(r.bytes_out)}</td>
                      <td data-status={r.status}><Badge kind={statusClass(r.status)}>{statusLabel(r.status)}</Badge></td>
                      <td className="mono small">{r.reason}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="load-more-row">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void loadMore()}
                disabled={logs.length < 50}
              >
                Load more
              </Button>
            </div>
          </>
        )
      )}
    </div>
  );
}
