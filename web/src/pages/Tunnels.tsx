import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, Copy } from "lucide-react";
import { toast } from "sonner";
import { apiFetch } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { Badge, Button, Dialog, ErrorNotice, Input, PageHeader, SkeletonRows } from "@/components/ds";
import type { AccessMode } from "@/lib/contract";
import { AccessModePanel, type AccessModePanelHandle } from "@/components/AccessModePanel";
import { Toaster } from "@/components/ui/sonner";

interface ConnectInfo { server: string }

interface Tunnel {
  id: string; name: string; type: string; remote_port: number;
  local_addr: string; bytes_in: number; bytes_out: number; connected: boolean;
  // v0.3.0 additive: present for http tunnels only.
  hostname?: string; access_mode?: AccessMode;
  // service_id is the durable-service id (http tunnels only). Required for
  // per-service routes (/services/{id}/access-mode, /api-keys); using the
  // per-session tunnel.id 404s.
  service_id?: string;
}

const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
};

export default function Tunnels() {
  const qc = useQueryClient();
  const [configure, setConfigure] = useState<Tunnel | null>(null);
  const panelRef = useRef<AccessModePanelHandle>(null);
  // SSE is primary; poll every 30 s as a fallback when SSE is unavailable.
  const { data, isLoading } = useQuery({
    queryKey: ["tunnels"],
    queryFn: () => apiFetch<Tunnel[]>("/tunnels"),
    refetchInterval: 30000,
  });
  // P1-2/P1-6: relay control endpoint surfaced so the REMOTE column on TCP
  // tunnels can offer a copy-of-the-real-thing (not just :port). 404 here is
  // tolerated — we fall back to window.location.host.
  const connectInfo = useQuery({
    queryKey: ["connect-info"],
    queryFn: () => apiFetch<ConnectInfo>("/clients/connect-info"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  const relayHost = (() => {
    const winHost = typeof window !== "undefined" ? window.location.hostname : "";
    const s = connectInfo.data?.server ?? "";
    if (!s) return winHost;
    // ":7000" (port-only, relay bound to all interfaces) carries no host —
    // fall back to the dashboard's own hostname so copying yields a real
    // host:port instead of ":7000:9003".
    const i = s.lastIndexOf(":");
    if (i <= 0) return winHost;
    return s.slice(0, i);
  })();
  // P2-2 — search filter + togglable sort column. Default sort matches P0-7
  // (type asc, name asc) so reload is stable; clicking a header toggles its
  // direction.
  const [q, setQ] = useState("");
  type SortKey = "name" | "type" | "status";
  const [sortKey, setSortKey] = useState<SortKey>("type");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("asc");
  function toggleSort(k: SortKey) {
    if (sortKey === k) setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    else { setSortKey(k); setSortDir("asc"); }
  }
  const sortIcon = (k: SortKey) => sortKey === k
    ? sortDir === "asc" ? <ArrowUp size={11} /> : <ArrowDown size={11} />
    : null;

  const sorted = useMemo(() => {
    const list = data ?? [];
    const filtered = q
      ? list.filter((t) =>
          `${t.name} ${t.type} ${t.local_addr} ${t.hostname ?? ""}`
            .toLowerCase()
            .includes(q.toLowerCase()))
      : list;
    const sgn = sortDir === "asc" ? 1 : -1;
    return [...filtered].sort((a, b) => {
      let cmp = 0;
      if (sortKey === "name") cmp = (a.name || "").localeCompare(b.name || "");
      else if (sortKey === "type") {
        cmp = a.type.localeCompare(b.type);
        if (cmp === 0) cmp = (a.name || "").localeCompare(b.name || "");
      } else {
        // status: connected first, disconnected after — tie-break by name.
        cmp = (Number(b.connected) - Number(a.connected));
        if (cmp === 0) cmp = (a.name || "").localeCompare(b.name || "");
      }
      return cmp * sgn;
    });
  }, [data, q, sortKey, sortDir]);

  useEffect(() => {
    // NOTE: EventSource requires same-origin (the Go server must serve this SPA).
    // jsdom (used by vitest/RTL) does not implement EventSource; skip gracefully.
    if (typeof EventSource === "undefined") return;
    const es = new EventSource("/api/v1/events");
    const onTunnels = () => qc.invalidateQueries({ queryKey: ["tunnels"] });
    es.addEventListener("tunnels", onTunnels);
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) {
        // Stream closed — session may have expired. Invalidate /me so the
        // centralized RequireAuth handler can redirect to /login if needed.
        es.close();
        qc.invalidateQueries({ queryKey: ["me"] });
      }
      // If readyState is CONNECTING the browser is auto-retrying; do nothing.
    };
    return () => {
      es.removeEventListener("tunnels", onTunnels);
      es.onerror = null;
      es.close();
    };
  }, [qc]);
  return (
    <div className="tunnels-page">
      <PageHeader title="Tunnels" />
      <ErrorNotice variant="info" role="note">
        Tunnels are the live connections a client is holding right now. Their saved config and
        access mode live in <Link to="/services">Services</Link>.
      </ErrorNotice>
      <div className="toolbar-row">
        <Input
          type="search"
          aria-label="Filter tunnels"
          placeholder="filter by name, type, address…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </div>
      {isLoading ? (
        <div className="table-wrap skel-pad">
          <SkeletonRows n={4} />
        </div>
      ) : !data || data.length === 0 ? (
        <div className="state-card">
          <div className="icon-bubble">
            <svg
              width={18}
              height={18}
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.7"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <circle cx="12" cy="4.5" r="2.5" />
              <path d="m10.2 6.3-3.9 3.9" />
              <circle cx="4.5" cy="12" r="2.5" />
              <path d="M7 12h10" />
              <circle cx="19.5" cy="12" r="2.5" />
              <path d="m13.8 17.7 3.9-3.9" />
              <circle cx="12" cy="19.5" r="2.5" />
            </svg>
          </div>
          <p>No live tunnels. Run <code>burrow connect</code> with a token.</p>
          <Link to="/clients/connect"><Button variant="primary" size="sm">Connect a client</Button></Link>
        </div>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Tunnels">
            <thead>
              <tr>
                <th>
                  <button
                    type="button"
                    className="sort-header"
                    onClick={() => toggleSort("name")}
                    aria-label={`Sort by name (${sortKey === "name" ? sortDir : "asc"})`}
                  >
                    Name {sortIcon("name")}
                  </button>
                </th>
                <th>
                  <button
                    type="button"
                    className="sort-header"
                    onClick={() => toggleSort("type")}
                    aria-label={`Sort by type (${sortKey === "type" ? sortDir : "asc"})`}
                  >
                    Type {sortIcon("type")}
                  </button>
                </th>
                <th>Remote</th>
                <th>Local</th>
                <th>Hostname</th>
                <th>Access</th>
                <th>Traffic</th>
                <th>
                  <button
                    type="button"
                    className="sort-header"
                    onClick={() => toggleSort("status")}
                    aria-label={`Sort by status (${sortKey === "status" ? sortDir : "asc"})`}
                  >
                    Status {sortIcon("status")}
                  </button>
                </th>
              </tr>
            </thead>
            <tbody>
              {sorted.map((t) => (
                <tr key={t.id}>
                  <td className={t.name ? "col-name" : "col-name muted-em"}>
                    {t.type === "http" && t.service_id ? (
                      <Link
                        to={`/services/${t.service_id}`}
                        aria-label={`Open service ${t.name}`}
                      >
                        {t.name || "—"}
                      </Link>
                    ) : (
                      t.name || "—"
                    )}
                  </td>
                  <td><Badge nodot>{t.type}</Badge></td>
                  <td className="col-remote">
                    {t.type === "http"
                      ? <span className="muted">—</span>
                      : (
                        <span className="row row-center gap-2">
                          <span className="mono">:{t.remote_port}</span>
                          <button
                            type="button"
                            className="icon-btn"
                            aria-label={`Copy endpoint ${relayHost}:${t.remote_port}`}
                            onClick={() => {
                              const ep = `${relayHost}:${t.remote_port}`;
                              void navigator.clipboard?.writeText(ep);
                              toast.success("Copied.");
                            }}
                          >
                            <Copy size={13} />
                          </button>
                        </span>
                      )}
                  </td>
                  <td className="col-local">{t.local_addr}</td>
                  <td>
                    {t.type === "http" && t.hostname ? (
                      <span className="row row-center gap-2">
                        <span className="mono">{t.hostname}</span>
                        <button
                          type="button"
                          className="icon-btn"
                          aria-label={`Copy hostname ${t.hostname}`}
                          onClick={() => void navigator.clipboard?.writeText(t.hostname!)}
                        >
                          <Copy size={13} />
                        </button>
                      </span>
                    ) : (
                      <span className="muted">—</span>
                    )}
                  </td>
                  <td>
                    <span className="row row-center gap-2">
                      <Badge kind={`access-${t.access_mode ?? "open"}`} nodot>
                        {ACCESS_LABEL[t.access_mode ?? "open"]}
                      </Badge>
                      {t.type === "http" && (
                        <Button variant="secondary" size="sm" onClick={() => setConfigure(t)}>
                          Configure
                        </Button>
                      )}
                    </span>
                  </td>
                  <td className="col-traffic small">
                    <span title={`In: ${t.bytes_in} bytes`}>↓ {formatBytes(t.bytes_in)}</span>
                    {"  "}
                    <span title={`Out: ${t.bytes_out} bytes`}>↑ {formatBytes(t.bytes_out)}</span>
                  </td>
                  <td>
                    {t.connected
                      ? <Badge kind="status-connected">connected</Badge>
                      : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={configure !== null}
        onOpenChange={(o) => { if (!o) setConfigure(null); }}
        size="lg"
        title={configure ? `Access · ${configure.name || configure.id}` : ""}
        description="Choose how Burrow gates requests before proxying to this service."
        footer={
          <>
            <Button variant="secondary" onClick={() => setConfigure(null)}>Cancel</Button>
            <Button
              variant="primary"
              onClick={() => panelRef.current?.save()}
            >
              Save changes
            </Button>
          </>
        }
      >
        {configure && (
          <AccessModePanel
            // Use the durable service id for http tunnels (per-service routes
            // 404 when given the per-session tunnel.id). Fall back to
            // configure.id for any legacy tcp/back-compat path.
            serviceId={configure.service_id ?? configure.id}
            serviceName={configure.name || configure.id}
            mode={configure.access_mode ?? "open"}
            panelRef={panelRef}
          />
        )}
      </Dialog>
      <Toaster />
    </div>
  );
}
