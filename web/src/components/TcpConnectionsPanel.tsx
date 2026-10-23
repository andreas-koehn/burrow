import { useId } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { RelativeTime } from "@/components/RelativeTime";
import type { ConnectionLog } from "@/lib/contract";

const STATUS: Record<ConnectionLog["status"], string> = {
  closed_clean: "closed",
  closed_error: "failed",
  closed_idle: "idle timeout",
  rejected: "refused",
};

function duration(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  return m < 60 ? `${m} min ${s % 60} s` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

/**
 * The connection log of a tcp service: visitors, forwards and refusals. A connection
 * enters the log when it ends; the open ones are in TcpLiveConnectionsPanel.
 */
export function TcpConnectionsPanel({ serviceId }: { serviceId: string }) {
  const titleId = useId();
  const logs = useQuery({
    queryKey: ["connection-logs", "service", serviceId],
    queryFn: () => apiFetch<ConnectionLog[]>(`/connection-logs?service_id=${encodeURIComponent(serviceId)}&limit=20`),
    refetchInterval: 15_000,
    // A hidden tab does not poll; the list is fetched again when the window regains focus.
    refetchIntervalInBackground: false,
    retry: false,
  });
  const denied = logs.error instanceof ApiError && logs.error.status === 403;
  const rows = logs.data ?? [];

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Recent connections</h2>
      <p className="muted small">
        Ended connections are listed here, newest first. The traffic log has the same rows, with filters and an export.
      </p>
      {denied ? (
        <p className="muted small">The connection log needs audit access, which your role does not have.</p>
      ) : logs.error ? (
        <p role="alert" className="notice-inline">Couldn&apos;t load the connections.</p>
      ) : rows.length === 0 ? (
        !logs.isLoading && <p className="muted small">Nothing has connected yet.</p>
      ) : (
        <div className="table-wrap">
          <table className="table" aria-label="Connections of this service">
            <thead>
              <tr><th>Source</th><th>Started</th><th>Duration</th><th>Traffic</th><th>Result</th></tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id}>
                  <td>
                    <span className="mono">{r.source_ip || "—"}</span>
                    {r.kind === "tcp_forward" && (
                      <span className="muted small"> · Forward{r.token_name ? ` · ${r.token_name}` : ""}</span>
                    )}
                  </td>
                  <td><RelativeTime value={r.started_at} /></td>
                  <td>{r.status === "rejected" ? "—" : duration(r.duration_ms)}</td>
                  <td>
                    <span title={`In: ${r.bytes_in} bytes`}>↓ {formatBytes(r.bytes_in)}</span>{"  "}
                    <span title={`Out: ${r.bytes_out} bytes`}>↑ {formatBytes(r.bytes_out)}</span>
                  </td>
                  <td>{STATUS[r.status] ?? r.status}{r.reason ? ` (${r.reason})` : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="small">
        <Link className="link-inline" to={`/traffic?service=${encodeURIComponent(serviceId)}`}>Open the traffic log</Link>
      </p>
    </section>
  );
}

