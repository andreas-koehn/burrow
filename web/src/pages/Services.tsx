import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, Copy } from "lucide-react";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { Button, Badge, Dialog, EmptyState, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, Segmented, Select, SkeletonRows } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { formatBytes } from "@/lib/format";
import type { Service, AccessMode, ClientDetail, ClientView } from "@/lib/contract";
import { ServiceUrl } from "@/components/ServiceUrl";
import { SlugField, slugError } from "@/components/SlugField";
import { AccessModePanel, type AccessModePanelHandle } from "@/components/AccessModePanel";

const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
};

const ACCESS_MODE_OPTIONS = [
  { value: "open",         label: "Open" },
  { value: "api_key",     label: "API key" },
  { value: "burrow_login", label: "Burrow login" },
];

interface ConnectInfo { server: string }

/** One live tunnel: what a client is holding open right now. */
interface Tunnel {
  id: string; name: string; type: string; remote_port: number;
  local_addr: string; bytes_in: number; bytes_out: number; connected: boolean;
  url?: string; access_mode?: AccessMode;
  // The durable service's id (http tunnels only); a tcp tunnel has no service row to join.
  service_id?: string;
}

/** A table row: a saved service (All), or a live tunnel joined to its service (Live). */
interface Row {
  key: string;
  name: string;
  type: string;
  connected: boolean;
  haystack: string;
  service?: Service;
  tunnel?: Tunnel;
}

const SHOW = [{ value: "all", label: "All" }, { value: "live", label: "Live" }] as const;

function ShowFilter({ live, onChange }: { live: boolean; onChange: (live: boolean) => void }) {
  return <Segmented aria-label="Show" options={SHOW} value={live ? "live" : "all"} onChange={(v) => onChange(v === "live")} />;
}

export default function Services() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // All | Live lives in the URL, so a reload or a shared link keeps the choice.
  const live = searchParams.get("live") === "1";
  const setLive = (on: boolean) => setSearchParams((prev) => {
    const next = new URLSearchParams(prev);
    if (on) next.set("live", "1"); else next.delete("live");
    return next;
  }, { replace: true });
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  // Live only. SSE is primary; poll every 30 s as a fallback when SSE is unavailable.
  const tunnels = useQuery({
    queryKey: ["tunnels"],
    queryFn: () => apiFetch<Tunnel[]>("/tunnels"),
    refetchInterval: 30000,
    retry: false,
    enabled: live,
  });
  // The relay's host, so a tcp row can copy a real host:port. A 404 is tolerated:
  // the dashboard's own hostname stands in.
  const connectInfo = useQuery({
    queryKey: ["connect-info"],
    queryFn: () => apiFetch<ConnectInfo>("/clients/connect-info"),
    retry: false,
    staleTime: 5 * 60_000,
    enabled: live,
  });
  const relayHost = (() => {
    const winHost = typeof window !== "undefined" ? window.location.hostname : "";
    const s = connectInfo.data?.server ?? "";
    // ":7000" (port only, relay bound to all interfaces) carries no host.
    const i = s.lastIndexOf(":");
    return i <= 0 ? winHost : s.slice(0, i);
  })();
  // Who holds each connection. A tunnel does not name its client; a client's detail
  // lists its tunnels by id. Both endpoints are admin only, so nobody else asks
  // (same gate as the sidebar's count in Layout) and sees "—" instead.
  const isAdmin = useAuth().user?.role === "admin";
  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    retry: false,
    refetchInterval: 30000,
    enabled: live && isAdmin,
  });
  // One request per connected client. A stream event refreshes the list above, so a
  // client that just connected gets its detail at once (a new query). The detail of a
  // client already listed is only polled: a tunnel it adds later shows "—" for up to 30 s.
  const clientDetails = useQueries({
    queries: (Array.isArray(clients.data) ? clients.data : []).map((c) => ({
      queryKey: ["client", c.session_id],
      queryFn: () => apiFetch<ClientDetail>(`/clients/${c.session_id}`),
      retry: false,
      refetchInterval: 30000,
      enabled: live && isAdmin,
    })),
  });
  const holders = new Map<string, { sessionId: string; name: string }>();
  for (const d of clientDetails) {
    for (const s of d.data?.services ?? []) holders.set(s.id, { sessionId: d.data!.session_id, name: d.data!.token_name });
  }

  useEffect(() => {
    // EventSource requires same-origin (the Go server serves this SPA); jsdom has none.
    if (!live || typeof EventSource === "undefined") return;
    const es = new EventSource("/api/v1/events");
    const onTunnels = () => {
      qc.invalidateQueries({ queryKey: ["tunnels"] });
      // The list only: the per-client details live under ["client", id] and keep their own interval (see above).
      qc.invalidateQueries({ queryKey: ["clients"] });
    };
    es.addEventListener("tunnels", onTunnels);
    es.onerror = () => {
      // CONNECTING means the browser is retrying by itself. CLOSED may mean the session
      // expired: asking for /me again lets RequireAuth send the visitor to /login.
      if (es.readyState === EventSource.CLOSED) {
        es.close();
        qc.invalidateQueries({ queryKey: ["me"] });
      }
    };
    return () => {
      es.removeEventListener("tunnels", onTunnels);
      es.onerror = null;
      es.close();
    };
  }, [qc, live]);

  // What the access dialog needs: a saved service, or a live http tunnel's service id when the join has no row.
  const [configure, setConfigure] = useState<Pick<Service, "id" | "name" | "access_mode"> & { type: string } | null>(null);
  const panelRef = useRef<AccessModePanelHandle>(null);

  // P2-2 — filter + sort for the Services table, in both All and Live.
  // Default sort: type asc, name asc.
  const [q, setQ] = useState("");
  type SortKey = "name" | "type" | "status";
  const [sortKey, setSortKey] = useState<SortKey>("type");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("asc");
  function toggleSort(k: SortKey) {
    if (sortKey === k) setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    else { setSortKey(k); setSortDir("asc"); }
  }
  const ariaSort = (k: SortKey) => sortKey === k ? (sortDir === "asc" ? "ascending" : "descending") : undefined;
  const sortIcon = (k: SortKey) => sortKey === k
    ? sortDir === "asc" ? <ArrowUp size={11} /> : <ArrowDown size={11} />
    : null;
  const tunnelList = tunnels.data;
  const filtered = useMemo(() => {
    const services = data ?? [];
    const byId = new Map(services.map((s) => [s.id, s]));
    const rows: Row[] = live
      ? (tunnelList ?? []).map((t) => {
          const s = t.service_id ? byId.get(t.service_id) : undefined;
          return {
            key: t.id, name: s?.name || t.name || s?.id || "", type: t.type, connected: t.connected, service: s, tunnel: t,
            haystack: `${t.name} ${s?.name ?? ""} ${t.type} ${t.local_addr} ${s?.slug ?? ""} ${s?.url || t.url || ""}`,
          };
        })
      : services.map((s) => ({
          key: s.id, name: s.name || s.id, type: s.type, connected: s.connected, service: s,
          haystack: `${s.name} ${s.type} ${s.slug ?? ""} ${s.url ?? ""}`,
        }));
    const f = q ? rows.filter((r) => r.haystack.toLowerCase().includes(q.toLowerCase())) : rows;
    const sgn = sortDir === "asc" ? 1 : -1;
    return [...f].sort((a, b) => {
      let cmp = 0;
      if (sortKey === "type") cmp = a.type.localeCompare(b.type);
      else if (sortKey === "status") cmp = Number(b.connected) - Number(a.connected);
      if (cmp === 0) cmp = a.name.localeCompare(b.name);
      return cmp * sgn;
    });
  }, [data, tunnelList, live, q, sortKey, sortDir]);

  // P2-1: minimal "New service" dialog. POST /services is admin-only on the
  // backend (v0.5.2 P3.6); 403 here surfaces a friendly message.
  const [newOpen, setNewOpen] = useState(false);
  const [nsServiceId, setNsServiceId] = useState("");
  const [nsTitle, setNsTitle] = useState("");
  const [nsSlug, setNsSlug] = useState("");
  const [nsAccessMode, setNsAccessMode] = useState("open");
  const [aiFlow, setAiFlow] = useState(false);
  const [nsErr, setNsErr] = useState<string | null>(null);

  // P5.2 — ?new=ai or ?new=1 auto-opens the dialog pre-filled.
  useEffect(() => {
    const newParam = searchParams.get("new");
    if (newParam === "ai") {
      setNewOpen(true);
      setNsAccessMode("api_key");
      setAiFlow(true);
    } else if (newParam === "1") {
      setNewOpen(true);
    }
    if (newParam !== null) {
      // Only `new` is consumed; the rest of the query (the Live filter) stays.
      setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("new"); return next; }, { replace: true });
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Burrow suggests a slug each time the dialog opens; the operator may replace it.
  const suggestion = useQuery({
    queryKey: ["slug-suggestion"],
    queryFn: () => apiFetch<{ slug: string }>("/services/slug-suggestion"),
    enabled: newOpen,
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });
  useEffect(() => {
    if (newOpen && suggestion.data && nsSlug === "") setNsSlug(suggestion.data.slug);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [newOpen, suggestion.data]);

  function closeNew() {
    setNewOpen(false);
    setNsServiceId("");
    setNsTitle("");
    setNsSlug("");
    setNsAccessMode("open");
    setAiFlow(false);
    setNsErr(null);
  }

  const createService = useMutation({
    mutationFn: () =>
      apiFetch<{ id: string; created_at: string }>("/services", {
        method: "POST",
        body: JSON.stringify({
          service_id: nsServiceId,
          title: nsTitle || undefined,
          slug: nsSlug || undefined,
          access_mode: nsAccessMode,
        }),
      }),
    onSuccess: async (resp) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      if (aiFlow && resp?.id) {
        // The service only becomes reachable under /ai/ once it has a provider.
        try {
          const p = await apiFetch<{ slug: string }>("/ai/providers", {
            method: "POST",
            body: JSON.stringify({ name: nsTitle || nsServiceId, kind: "tunnel", service_id: resp.id }),
          });
          qc.invalidateQueries({ queryKey: ["ai", "providers"] });
          closeNew();
          nav(`/gateway/providers/${p.slug}`);
        } catch (e) {
          // The service exists, the provider does not. Say why and stay here:
          // this page's toaster would not survive a navigation.
          const why = e instanceof ApiError ? e.message : "the request failed";
          // The only pointer to the next step: keep it up longer and make it the way there.
          toast.error(`Service ${resp.id} created, but it was not registered as a provider: ${why}. Add it under Providers.`, {
            duration: 20_000,
            action: { label: "Open Providers", onClick: () => nav("/gateway/providers") },
          });
          closeNew();
        }
      } else {
        toast.success(`Service ${nsServiceId} created.`);
        closeNew();
      }
    },
    onError: (e: unknown) => {
      if (e instanceof ApiError && e.status === 403) {
        setNsErr("You don't have permission to create services.");
      } else if (e instanceof ApiError) {
        setNsErr(e.message);
      } else {
        setNsErr("Couldn't create service.");
      }
    },
  });

  // Live lists tunnels and only decorates them with the saved service: it fails on its own query alone.
  const failure = live ? tunnels.error : error;

  return (
    <div className="services-page">
      <PageHeader
        title="Services"
        subtitle="Durable services exposed through this relay, with their access configuration."
        actions={<Button variant="primary" size="sm" onClick={() => { setNewOpen(true); setNsErr(null); }}>New service</Button>}
      />
      <ErrorNotice variant="info" role="note">
        Services are the saved configuration. Switch to Live to see what is connected right now.
      </ErrorNotice>

      {/* Always there, also while Live loads or fails: it is the way back to All. */}
      <div className="toolbar-row">
        <ShowFilter live={live} onChange={setLive} />
        <Input
          type="search"
          aria-label="Filter services"
          placeholder={live ? "filter by name, type, address…" : "filter by name, type, URL…"}
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </div>
      {failure ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void (live ? tunnels.refetch() : refetch())}>Retry</Button>}
        >
          Couldn't load services: {failure instanceof ApiError ? failure.message : "Unknown error"}
        </ErrorNotice>
      ) : (live ? tunnels.isLoading : isLoading) ? (
        <div className="table-wrap skel-pad">
          <SkeletonRows n={4} />
        </div>
      ) : live && (tunnelList ?? []).length === 0 ? (
        <EmptyState
          title="Nothing is live right now"
          action={<Link className="btn btn-primary btn-sm" to="/clients/connect">Connect a client</Link>}
        >
          A service goes live when a client runs <code>burrow connect</code> for it.
        </EmptyState>
      ) : !live && (data ?? []).length === 0 ? (
        <EmptyState title="No services yet">
          Run <code>burrow connect</code> with <code>--type http</code> to expose a service.
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Services">
            <thead>
              <tr>
                <th aria-sort={ariaSort("name")}>
                  <button type="button" className="sort-header" onClick={() => toggleSort("name")}
                    aria-label={`Sort by name (${sortKey === "name" ? sortDir : "asc"})`}>
                    Name {sortIcon("name")}
                  </button>
                </th>
                <th aria-sort={ariaSort("type")}>
                  <button type="button" className="sort-header" onClick={() => toggleSort("type")}
                    aria-label={`Sort by type (${sortKey === "type" ? sortDir : "asc"})`}>
                    Type {sortIcon("type")}
                  </button>
                </th>
                <th>URL</th>
                <th>Access</th>
                {live && <th>Client</th>}
                {live && <th>Local</th>}
                {live && <th>Remote</th>}
                {live && <th>Traffic</th>}
                <th aria-sort={ariaSort("status")}>
                  <button type="button" className="sort-header" onClick={() => toggleSort("status")}
                    aria-label={`Sort by status (${sortKey === "status" ? sortDir : "asc"})`}>
                    Status {sortIcon("status")}
                  </button>
                </th>
                <th className="col-actions"></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map(({ key, name, type, connected, service: s, tunnel: t }) => {
                const serviceId = s?.id ?? t?.service_id;
                const access = s?.access_mode ?? t?.access_mode ?? "open";
                const holder = t ? holders.get(t.id) : undefined;
                const target = s ?? (t && type === "http" && t.service_id
                  ? { id: t.service_id, name, type, access_mode: access }
                  : undefined);
                return (
              <tr key={key}>
                <td className="col-name link-row">
                  {serviceId ? <Link to={`/services/${serviceId}`}>{name}</Link> : name || "—"}
                </td>
                <td><Badge kind={`type-${type}`} nodot>{type}</Badge></td>
                <td>
                  {type === "http" ? <ServiceUrl slug={s?.slug ?? ""} url={s?.url || t?.url} /> : <span className="muted">—</span>}
                </td>
                <td><Badge kind={`access-${access}`} nodot>{ACCESS_LABEL[access]}</Badge></td>
                {t && (
                  <>
                    <td>
                      {holder ? <Link to={`/clients/${holder.sessionId}`}>{holder.name}</Link> : <span className="muted">—</span>}
                    </td>
                    <td className="col-local">{t.local_addr}</td>
                    <td className="col-remote">
                      {type === "http" ? <span className="muted">—</span> : (
                        <span className="row row-center gap-2">
                          <span className="mono">:{t.remote_port}</span>
                          <button
                            type="button"
                            className="icon-btn"
                            aria-label={`Copy endpoint ${relayHost}:${t.remote_port}`}
                            onClick={() => {
                              void navigator.clipboard?.writeText(`${relayHost}:${t.remote_port}`);
                              toast.success("Copied.");
                            }}
                          >
                            <Copy size={13} />
                          </button>
                        </span>
                      )}
                    </td>
                    <td className="col-traffic small">
                      <span title={`In: ${t.bytes_in} bytes`}>↓ {formatBytes(t.bytes_in)}</span>
                      {"  "}
                      <span title={`Out: ${t.bytes_out} bytes`}>↑ {formatBytes(t.bytes_out)}</span>
                    </td>
                  </>
                )}
                <td>
                  {!connected
                    ? <Badge kind="status-idle">idle</Badge>
                    : live
                      ? <Badge kind="status-connected">connected</Badge>
                      : <Link to="/services?live=1" aria-label={`View live tunnel for ${name}`}><Badge kind="status-connected">connected</Badge></Link>}
                </td>
                <td className="col-actions">
                  {target && <Button variant="secondary" size="sm" onClick={() => setConfigure(target)}>Configure</Button>}
                </td>
              </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={configure !== null}
        onOpenChange={(o) => { if (!o) setConfigure(null); }}
        size="lg"
        title={configure ? `Access · ${configure.name || configure.id}` : ""}
        description={configure?.type === "tcp"
          ? "Raw TCP service — only Open passthrough applies."
          : "Choose how Burrow gates requests before proxying to this service."}
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
            serviceId={configure.id}
            serviceName={configure.name || configure.id}
            mode={configure.access_mode}
            clientId={`svc:${configure.id}`}
            panelRef={panelRef}
          />
        )}
      </Dialog>

      {/* P2-1 / P5.1 — new-service dialog */}
      <Dialog
        open={newOpen}
        onOpenChange={(o) => { if (!o) closeNew(); }}
        title={aiFlow ? "New AI service" : "New service"}
        description={aiFlow
          ? "Creates a service with API-key access and registers it as a model provider."
          : "Pre-provision a service so a connecting client adopts the same id."}
        footer={
          <>
            <Button variant="secondary" onClick={closeNew}>Cancel</Button>
            <Button
              variant="primary"
              disabled={!nsServiceId || slugError(nsSlug) !== null || createService.isPending}
              onClick={() => createService.mutate()}
            >
              {createService.isPending ? "Creating…" : aiFlow ? "Create and continue" : "Create"}
            </Button>
          </>
        }
      >
        <FormFieldGroup>
          <FormField label="Service ID" htmlFor="ns-service-id" w="md">
            <Input id="ns-service-id" className="mono" placeholder="e.g. web-prod (a-z 0-9 - _, 3–64)" value={nsServiceId} onChange={(e) => setNsServiceId(e.target.value)} />
          </FormField>
          <FormField label="Title" htmlFor="ns-title" w="md">
            <Input id="ns-title" placeholder="optional display name" value={nsTitle} onChange={(e) => setNsTitle(e.target.value)} />
          </FormField>
          <SlugField id="ns-slug" value={nsSlug} onChange={setNsSlug} />
          {!aiFlow && (
            <FormField label="Access mode" htmlFor="ns-access-mode" w="md">
              <Select
                id="ns-access-mode"
                options={ACCESS_MODE_OPTIONS}
                value={nsAccessMode}
                onChange={setNsAccessMode}
              />
            </FormField>
          )}
        </FormFieldGroup>
        <p className="muted small">Apps that load assets from absolute paths (/assets/…) need base-path support to work under a /svc/ URL.</p>
        {nsErr && <p role="alert" className="notice-inline error">{nsErr}</p>}
      </Dialog>
      <Toaster />
    </div>
  );
}
