import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { formatBytes } from "@/lib/format";
import { Input, Badge, PageHeader, SkeletonRows, Tabs } from "@/components/ds";
import { ClientTokensPanel } from "@/components/ClientTokensPanel";
import type { ClientDiscovery, ClientView } from "@/lib/contract";

/** MAJOR.MINOR.PATCH with an optional v; anything else (develop, a commit) has no order. */
function versionNumbers(v: string): [number, number, number] | null {
  const m = /^v?(\d{1,9})\.(\d{1,9})\.(\d{1,9})$/.exec(v.trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

/** True only when both are release versions and the client's is the lower one. */
function olderThan(client: string, relay: string | undefined): boolean {
  const c = versionNumbers(client);
  const r = relay === undefined ? null : versionNumbers(relay);
  if (!c || !r) return false;
  for (let i = 0; i < 3; i++) {
    if (c[i] !== r[i]) return c[i]! < r[i]!;
  }
  return false;
}

function ClientList() {
  const [q, setQ] = useState("");
  // The relay's own version, to mark clients that are behind it. Without an answer nothing is marked.
  const relay = useQuery({
    queryKey: ["client-discovery"],
    queryFn: () => apiFetch<ClientDiscovery>("/client/discovery"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  const relayVersion = relay.data?.version;
  const { data, isLoading, error } = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    retry: false,
    refetchInterval: 30000,
  });

  if (error) {
    return (
      <div className="notice-block error">
        <div className="icon-bubble"><AlertTriangle size={18} /></div>
        <p role="alert">Couldn't load clients: {error instanceof ApiError ? error.message : "Unknown error"}</p>
      </div>
    );
  }

  const rows = (data ?? []).filter((c) =>
    `${c.token_name} ${c.session_id}`.toLowerCase().includes(q.toLowerCase()));

  return (
    <>
      <div className="toolbar-row">
        <Input type="search" role="searchbox" aria-label="Search clients" placeholder="search by client or token…" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>
      {isLoading ? (
        <div className="table-wrap"><SkeletonRows n={5} /></div>
      ) : rows.length === 0 ? (
        <div className="state-card"><p>No clients connected</p></div>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Clients">
            <thead><tr><th>Client</th><th>Platform</th><th>Version</th><th>Remote IP</th><th>Status</th><th>Services</th><th>Traffic</th><th className="col-actions"></th></tr></thead>
            <tbody>
              {rows.map((c) => (
                <tr key={c.session_id}>
                  <td>{c.token_name}<div className="muted mono">{c.session_id}</div></td>
                  <td>{c.os} · {c.arch}</td>
                  <td>
                    <span className="row row-center gap-2">
                      <span className="mono">{c.client_version || "—"}</span>
                      {olderThan(c.client_version, relayVersion) && (
                        <Badge kind="status-idle" nodot>
                          update available<span className="visually-hidden"> (older than the relay)</span>
                        </Badge>
                      )}
                    </span>
                  </td>
                  <td className="col-created">{c.remote_addr}</td>
                  {/* P1-8 — every row of /clients comes from the live session
                      registry, so presence in the list IS the "connected"
                      signal. When the backend later exposes last_seen the
                      column can show relative time for disconnected clients. */}
                  <td><Badge kind="status-connected">connected</Badge></td>
                  <td><Link to={`/clients/${c.session_id}`} aria-label={`View ${c.service_count} services for ${c.token_name}`}><Badge kind="" nodot>{c.service_count}</Badge></Link></td>
                  <td className="col-created">↓{formatBytes(c.total_bytes_in)} ↑{formatBytes(c.total_bytes_out)}</td>
                  <td className="col-actions"><Link className="btn btn-secondary btn-sm" to={`/clients/${c.session_id}`}>View</Link></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

type ClientsTab = "clients" | "tokens";

export default function Clients() {
  // The tab lives in the URL, so /clients?tab=tokens can be linked and reloaded.
  const [searchParams, setSearchParams] = useSearchParams();
  const { user, loading } = useAuth();
  // The clients list is admin only; without a tab in the URL everyone else starts on their tokens.
  const fallback: ClientsTab = user?.role === "admin" ? "clients" : "tokens";
  const asked = searchParams.get("tab");
  const tab: ClientsTab = asked === "tokens" || asked === "clients" ? asked : fallback;
  // The URL names a tab only when it is not the one this user starts on.
  const setTab = (value: string) => setSearchParams((prev) => {
    const next = new URLSearchParams(prev);
    if (value === fallback) next.delete("tab"); else next.set("tab", value);
    return next;
  }, { replace: true });
  // Which tab to start on is not known before the user is.
  const undecided = loading && asked !== "tokens" && asked !== "clients";

  return (
    <div className="users-page">
      <PageHeader
        title="Clients"
        subtitle="Machines running burrow connected to this relay, and the tokens they connect with."
        actions={<Link className="btn btn-primary btn-sm" to="/clients/connect">Connect a client</Link>}
      />
      {!undecided && (
        <Tabs
          value={tab}
          onChange={setTab}
          tabs={[
            // The tokens are everyone's own, so a failure of the clients list stays inside its tab.
            { value: "clients", label: "Clients", content: <ClientList /> },
            { value: "tokens", label: "Tokens", content: <ClientTokensPanel /> },
          ]}
        />
      )}
    </div>
  );
}
