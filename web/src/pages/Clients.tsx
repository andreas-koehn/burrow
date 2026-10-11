import { Fragment, useId, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ChevronDown, ChevronRight } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { formatRelativeTime, formatTimestamp } from "@/lib/format";
import { Input, Badge, Button, EmptyState, PageHeader, SkeletonRows, Tabs } from "@/components/ds";
import { StatusBadge } from "@/components/StatusBadge";
import { ClientTokensPanel } from "@/components/ClientTokensPanel";
import { InstallLines } from "@/components/InstallLines";
import { useKnownClients } from "@/lib/useKnownClients";
import type { ClientDiscovery, KnownClient, Service } from "@/lib/contract";

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

/**
 * Where a served name leads: to the provider the service backs, else to the
 * service. Nothing when no service has that name, or more than one has (an
 * admin sees every owner's services, and a name alone does not say whose).
 */
function servedTarget(name: string, services: Service[]): string | undefined {
  const named = services.filter((s) => s.name === name);
  if (named.length !== 1) return undefined;
  const svc = named[0]!;
  return svc.provider_slug ? `/gateway/providers/${svc.provider_slug}` : `/services/${svc.id}`;
}

function Serves({ client, services }: { client: KnownClient; services: Service[] }) {
  if (client.served.length === 0) {
    // A relay that does not name what a session serves still counts it.
    const n = client.sessions.reduce((sum, s) => sum + s.service_count, 0);
    const first = client.sessions[0];
    if (!first || n === 0) return <>—</>;
    return <Link className="link-inline" to={`/clients/${first.session_id}`}>{n} {n === 1 ? "service" : "services"}</Link>;
  }
  return (
    <>
      {client.served.map((name, i) => {
        const to = servedTarget(name, services);
        return (
          <Fragment key={name}>
            {i > 0 && ", "}
            {to ? <Link className="link-inline" to={to}>{name}</Link> : name}
          </Fragment>
        );
      })}
    </>
  );
}

function LastSeen({ client }: { client: KnownClient }) {
  if (client.status === "online") return <>now</>;
  if (!client.last_seen) return <>—</>;
  // Relative as the value, the exact UTC time on hover: as "Last used" on the Tokens tab.
  return <time dateTime={client.last_seen} title={formatTimestamp(client.last_seen)}>{formatRelativeTime(client.last_seen)}</time>;
}

/** The live sessions of a token that is connected more than once. */
function Sessions({ client }: { client: KnownClient }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const label = `${client.sessions.length} sessions`;
  return (
    <div>
      <Button
        variant="ghost"
        size="sm"
        aria-label={`${label} of ${client.token_name}`}
        aria-expanded={open}
        aria-controls={id}
        icon={open ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
        onClick={() => setOpen(!open)}
      >
        {label}
      </Button>
      <ul id={id} role="list" className="client-sessions" hidden={!open}>
        {open && client.sessions.map((s) => (
          <li key={s.session_id}>
            <Link className="link-inline mono" to={`/clients/${s.session_id}`}>{s.session_id}</Link>{" "}
            <span className="muted mono">{s.remote_addr}</span>
          </li>
        ))}
      </ul>
    </div>
  );
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
  // The same rows, from the same request, as the sidebar's count.
  const { rows: data, isLoading, error } = useKnownClients({ refetchInterval: 30000 });
  // Same key and fetch as the shell. Only the links depend on it: without an answer the names stay text.
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const servicesList = Array.isArray(services.data) ? services.data : [];

  if (error) {
    return (
      <div className="notice-block error">
        <div className="icon-bubble"><AlertTriangle size={18} /></div>
        <p role="alert">Couldn't load clients: {error instanceof ApiError ? error.message : "Unknown error"}</p>
      </div>
    );
  }

  const known = data ?? [];
  if (!isLoading && known.length === 0) {
    return (
      <div className="stack-md">
        <EmptyState
          title="No clients yet"
          action={<Link className="btn btn-primary btn-sm" to="/clients/connect">Connect a client</Link>}
        >
          A client is a machine running burrow. Run these three lines on it, and it appears here.
        </EmptyState>
        <InstallLines relayOrigin={typeof window !== "undefined" ? window.location.origin : ""} />
      </div>
    );
  }

  const needle = q.trim().toLowerCase();
  const rows = known.filter((c) => [c.token_name, ...c.served].join(" ").toLowerCase().includes(needle));

  return (
    <>
      <div className="toolbar-row">
        <Input type="search" role="searchbox" aria-label="Search clients" placeholder="search by client or what it serves…" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>
      {isLoading ? (
        <div className="table-wrap"><SkeletonRows n={5} /></div>
      ) : rows.length === 0 ? (
        <div className="state-card"><p>No client matches “{q.trim()}”.</p></div>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Clients">
            <thead><tr><th>Client</th><th>Status</th><th>Last seen</th><th>Platform</th><th>Version</th><th>Serves</th><th className="col-actions"></th></tr></thead>
            <tbody>
              {rows.map((c) => {
                // The newest session; none unless the client is online.
                const session = c.sessions[0];
                return (
                  // A live session whose token was revoked has no token id.
                  <tr key={c.token_id || session?.session_id || c.token_name}>
                    <td>
                      {c.token_name}
                      {/* One address at most: of the live session, or of the last one. Several sessions list their own. */}
                      {c.sessions.length > 1
                        ? <Sessions client={c} />
                        : c.remote_addr && <div className="muted mono">{c.remote_addr}</div>}
                    </td>
                    <td><StatusBadge state={c.status} /></td>
                    <td className="col-created"><LastSeen client={c} /></td>
                    <td>{c.os || c.arch ? `${c.os} · ${c.arch}` : "—"}</td>
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
                    <td><Serves client={c} services={servicesList} /></td>
                    <td className="col-actions">
                      {session && <Link className="btn btn-secondary btn-sm" to={`/clients/${session.session_id}`}>View</Link>}
                    </td>
                  </tr>
                );
              })}
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
        subtitle="Machines running burrow that connect to this relay, and the tokens they connect with."
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
