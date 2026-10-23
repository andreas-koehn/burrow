import { useEffect, useMemo, useRef, useState, type MouseEvent } from "react";
import { Link, Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, Copy } from "lucide-react";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Badge, Dialog, Disclosure, EmptyState, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, Segmented, Select, SkeletonRows, SubmitHint, firstMissing } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { formatBytes } from "@/lib/format";
import { ACCESS_LABEL, forwardCommand, matchTcpService } from "@/lib/tcpAccess";
import { useRelayHost } from "@/lib/useRelayHost";
import type { Service, AccessMode, ClientDetail, ClientView } from "@/lib/contract";
import { ServiceUrl } from "@/components/ServiceUrl";
import { SlugField, slugError } from "@/components/SlugField";
import { AccessModePanel, type AccessModePanelHandle } from "@/components/AccessModePanel";
import { StatusBadge } from "@/components/StatusBadge";
import { serviceState, type ServedState } from "@/lib/status";
import { SERVICE_ID_HINT, deriveServiceId, deriveSlug, serviceIdError } from "@/lib/slug";
import { serviceUrl } from "@/lib/serviceUrl";
import { useCan } from "@/auth/useCan";
import { useServicesDomain } from "@/lib/useServicesDomain";
import { P } from "@/auth/permissions";

const ACCESS_MODE_OPTIONS = [
  { value: "open",         label: "Open" },
  { value: "api_key",     label: "API key" },
  { value: "burrow_login", label: "Burrow login" },
];

/** One live tunnel: what a client is holding open right now. */
interface Tunnel {
  id: string; name: string; type: string; remote_port: number;
  local_addr: string; bytes_in: number; bytes_out: number; connected: boolean;
  url?: string; access_mode?: AccessMode;
  // The durable service's id (http tunnels only); a tcp tunnel has no service row to join.
  service_id?: string;
}

/** A table row: a saved service (All), or a live tunnel joined to its service (Online). */
interface Row {
  key: string;
  name: string;
  type: string;
  state: ServedState;
  haystack: string;
  service?: Service;
  tunnel?: Tunnel;
}

/** Sorting by status: what is served now comes first. */
const STATE_ORDER: Record<ServedState, number> = { online: 0, offline: 1, never_connected: 2 };

const SHOW = [{ value: "all", label: "All" }, { value: "live", label: "Online" }] as const;

// A row opens its service on a click anywhere, except on what has a click of its own.
const OWN_CLICK = "a, button, input, select, textarea, [role='button']";

function ShowFilter({ live, onChange }: { live: boolean; onChange: (live: boolean) => void }) {
  return <Segmented aria-label="Show" options={SHOW} value={live ? "live" : "all"} onChange={(v) => onChange(v === "live")} />;
}

export default function Services() {
  const qc = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const navigate = useNavigate();
  // All | Online lives in the URL, so a reload or a shared link keeps the choice.
  // The parameter kept its name from when the choice read "Live": old links still work.
  const live = searchParams.get("live") === "1";
  // Services that answer through the AI gateway only are listed on request: they
  // have no address here, and their page is the provider's.
  const ai = searchParams.get("ai") === "1";
  const setFlag = (name: "live" | "ai", on: boolean) => setSearchParams((prev) => {
    const next = new URLSearchParams(prev);
    if (on) next.set(name, "1"); else next.delete(name);
    return next;
  }, { replace: true });
  const setLive = (on: boolean) => setFlag("live", on);
  const setAi = (on: boolean) => setFlag("ai", on);
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  // Online only. SSE is primary; poll every 30 s as a fallback when SSE is unavailable.
  const tunnels = useQuery({
    queryKey: ["tunnels"],
    queryFn: () => apiFetch<Tunnel[]>("/tunnels"),
    refetchInterval: 30000,
    retry: false,
    enabled: live,
  });
  // The relay's host, so a tcp row can copy a real host:port. Asked for in the
  // Online view, and in All once a tcp service is listed.
  const hasTcp = (data ?? []).some((x) => x.type === "tcp");
  const relayHost = useRelayHost(live || hasTcp);
  // Who holds each connection. A tunnel does not name its client; a client's detail
  // lists its tunnels by id. Both endpoints need tunnels:read:any, so nobody without it asks
  // (same gate as the sidebar's count in Layout) and sees "—" instead.
  const canListClients = useCan(P.tunnelsReadAny);
  // POST /services: services:configure:any.
  const canCreate = useCan(P.servicesConfigureAny);
  const { domain: servicesDomain } = useServicesDomain();
  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    retry: false,
    refetchInterval: 30000,
    enabled: live && canListClients,
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
      enabled: live && canListClients,
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

  // P2-2 — filter + sort for the Services table, in both All and Online.
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
  const { filtered, onlyGateway } = useMemo(() => {
    const services = data ?? [];
    const byId = new Map(services.map((s) => [s.id, s]));
    const rows: Row[] = live
      ? (tunnelList ?? []).map((t) => {
          // A tunnel that names its service is joined by id; a tcp one without it by name, when only one tcp service has it.
          const s = t.service_id ? byId.get(t.service_id) : matchTcpService(services, t);
          return {
            key: t.id, name: s?.name || t.name || s?.id || "", type: t.type, service: s, tunnel: t,
            // A live tunnel is held by a client right now.
            state: t.connected ? "online" as const : "offline" as const,
            haystack: `${t.name} ${s?.name ?? ""} ${t.type} ${t.local_addr} ${s?.slug ?? ""} ${s?.url || t.url || ""}`,
          };
        })
      : services.map((s) => ({
          key: s.id, name: s.name || s.id, type: s.type, state: serviceState(s), service: s,
          haystack: `${s.name} ${s.type} ${s.slug ?? ""} ${s.url ?? ""}`,
        }));
    const listed = ai ? rows : rows.filter((r) => !r.service?.gateway_only);
    const f = q ? listed.filter((r) => r.haystack.toLowerCase().includes(q.toLowerCase())) : listed;
    const sgn = sortDir === "asc" ? 1 : -1;
    const sorted = [...f].sort((a, b) => {
      let cmp = 0;
      if (sortKey === "type") cmp = a.type.localeCompare(b.type);
      else if (sortKey === "status") cmp = STATE_ORDER[a.state] - STATE_ORDER[b.state];
      if (cmp === 0) cmp = a.name.localeCompare(b.name);
      return cmp * sgn;
    });
    // There are rows, and the AI filter alone keeps every one of them back.
    return { filtered: sorted, onlyGateway: rows.length > 0 && listed.length === 0 };
  }, [data, tunnelList, live, ai, q, sortKey, sortDir]);

  function openRow(e: MouseEvent<HTMLTableRowElement>, serviceId: string) {
    if ((e.target as Element).closest(OWN_CLICK)) return;
    // A click with a key held asks for a new tab or window, which only the name's link can open.
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    // Marking a URL to copy it ends in a click too.
    if (window.getSelection()?.toString()) return;
    navigate(`/services/${encodeURIComponent(serviceId)}`);
  }

  // "New service" asks for one name. The id, the URL slug and the title follow
  // it; under Advanced each can be given a value of its own. POST /services is
  // admin-only on the backend (v0.5.2 P3.6); 403 here surfaces a friendly message.
  const [newOpen, setNewOpen] = useState(false);
  const [nsName, setNsName] = useState("");
  // null: the field follows the name.
  const [nsServiceId, setNsServiceId] = useState<string | null>(null);
  const [nsSlug, setNsSlug] = useState<string | null>(null);
  const [nsTitle, setNsTitle] = useState<string | null>(null);
  const [nsAccessMode, setNsAccessMode] = useState("open");
  const [nsAdvanced, setNsAdvanced] = useState(false);
  // A field's own rule is shown once the field was left, not while it is typed in.
  const [nsNameLeft, setNsNameLeft] = useState(false);
  const [nsIdLeft, setNsIdLeft] = useState(false);
  // What the relay said about one field; nsErr is everything else.
  const [nsNameErr, setNsNameErr] = useState<string | null>(null);
  const [nsIdErr, setNsIdErr] = useState<string | null>(null);
  const [nsSlugErr, setNsSlugErr] = useState<string | null>(null);
  const [nsErr, setNsErr] = useState<string | null>(null);

  // A service with its provider is made under Providers; the old way in, the
  // parameter new with the value "ai", is sent there (see the return below).
  const toGuidedProvider = searchParams.get("new") === "ai";

  // ?new=1 auto-opens the dialog.
  useEffect(() => {
    // The redirect is on its way: taking the parameter out here would undo it.
    if (toGuidedProvider) return;
    const newParam = searchParams.get("new");
    if (newParam === "1") setNewOpen(true);
    if (newParam !== null) {
      // Only `new` is consumed; the rest of the query (the filters) stays.
      setSearchParams((prev) => { const next = new URLSearchParams(prev); next.delete("new"); return next; }, { replace: true });
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const derivedId = deriveServiceId(nsName);
  const derivedSlug = deriveSlug(nsName);
  // Burrow suggests a slug only where the name gives none (a name without a
  // letter or digit, whose id is typed under Advanced).
  const suggestion = useQuery({
    queryKey: ["slug-suggestion"],
    queryFn: () => apiFetch<{ slug: string }>("/services/slug-suggestion"),
    enabled: newOpen && nsName.trim() !== "" && derivedSlug === "" && !nsSlug,
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });
  const followedSlug = derivedSlug || suggestion.data?.slug || "";
  // An emptied field counts as following the name.
  const nsId = nsServiceId || derivedId;
  const nsUrlSlug = nsSlug || followedSlug;
  const nsShownTitle = (nsTitle || nsName).trim();

  const nsNameMissing = nsName.trim() === "";
  // The name has to give an id the relay takes, unless the id is typed.
  const nsNameProblem = nsNameMissing || nsServiceId ? null
    : derivedId === "" ? "Use letters or digits."
    : derivedId.length < 3 ? "Use at least three letters or digits."
    : null;
  const nsIdProblem = nsServiceId ? serviceIdError(nsServiceId) : null;
  // The slug that will be sent, typed or not: with a typed id, a short name
  // ("DB") still gives a slug the relay refuses. Where the name itself is
  // what is wrong, the slug that follows it is not named a second time.
  const nsSlugProblem = nsNameProblem !== null && !nsSlug ? null : slugError(nsUrlSlug);
  const nsMissing = [
    nsNameMissing && "Enter a name.",
    nsNameProblem,
    nsIdProblem && "Check the service ID.",
    nsSlugProblem && "Check the URL slug.",
  ];
  const nsHint = firstMissing(nsMissing);
  const nsNameError = nsNameErr ?? (nsNameLeft ? nsNameProblem : null);
  const nsIdError = nsIdErr ?? (nsIdLeft ? nsIdProblem : null);
  // A field with an error is never left hidden.
  const nsAdvancedForced = nsIdProblem !== null || nsSlugProblem !== null;
  const nsAdvancedOpen = nsAdvanced || nsAdvancedForced;

  // The relay refused one field: it takes the focus. Where the field is under
  // Advanced, the same answer has opened it.
  useEffect(() => {
    const field = nsNameErr !== null ? "ns-name" : nsIdErr !== null ? "ns-service-id" : nsSlugErr !== null ? "ns-slug" : null;
    if (field) document.getElementById(field)?.focus();
  }, [nsNameErr, nsIdErr, nsSlugErr]);

  function closeNew() {
    setNewOpen(false);
    setNsName("");
    setNsServiceId(null);
    setNsSlug(null);
    setNsTitle(null);
    setNsAccessMode("open");
    setNsAdvanced(false);
    setNsNameLeft(false);
    setNsIdLeft(false);
    clearNsErrors();
  }
  function clearNsErrors() {
    setNsNameErr(null);
    setNsIdErr(null);
    setNsSlugErr(null);
    setNsErr(null);
  }

  const createService = useMutation({
    mutationFn: () =>
      apiFetch<{ id: string; created_at: string }>("/services", {
        method: "POST",
        body: JSON.stringify({
          service_id: nsId,
          title: nsShownTitle || undefined,
          slug: nsUrlSlug || undefined,
          access_mode: nsAccessMode,
        }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.success(`Service ${nsShownTitle || nsId} created.`);
      closeNew();
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setNsErr("Couldn't create service.");
      else if (e.status === 403) setNsErr("You don't have permission to create services.");
      // The relay names the id by its field ("service_id must match …"). A
      // field under Advanced is shown with what the relay said about it, and
      // stays shown while it is corrected.
      else if (e.status === 400 && e.message.startsWith("service_id")) { setNsIdErr(e.message); setNsAdvanced(true); }
      else if (e.status === 400 && e.message.startsWith("slug")) { setNsSlugErr(e.message); setNsAdvanced(true); }
      // The relay answers one 409 for a taken id, slug or title ("service id or
      // slug already in use"). While the id and the slug follow the name,
      // another name is the way out. Once one of them was typed it may be the
      // one that is taken, and the slug is the one a reader changes without a
      // second thought. The relay's other 409 (Burrow login without an auth
      // domain) is no field's.
      else if (e.status === 409 && e.message.includes("slug")) {
        if (nsServiceId || nsSlug) { setNsSlugErr(e.message); setNsAdvanced(true); }
        else setNsNameErr("A service with this name exists already.");
      }
      else setNsErr(e.message);
    },
  });

  // Online lists tunnels and only decorates them with the saved service: it fails on its own query alone.
  const failure = live ? tunnels.error : error;

  if (toGuidedProvider) return <Navigate to="/gateway/providers?new=local" replace />;

  return (
    <div className="services-page">
      <PageHeader
        title="Services"
        subtitle="Durable services exposed through this relay, with their access configuration."
        actions={canCreate ? <Button variant="primary" size="sm" onClick={() => setNewOpen(true)}>New service</Button> : undefined}
      />
      {/* Always there, also while Online loads or fails: it is the way back to All. */}
      <div className="toolbar-row">
        <ShowFilter live={live} onChange={setLive} />
        <Button
          variant="secondary"
          size="sm"
          aria-pressed={ai}
          title="Also list services that answer through the AI gateway only"
          onClick={() => setAi(!ai)}
        >
          AI
        </Button>
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
          title="Nothing is online right now"
          action={<Link className="btn btn-primary btn-sm" to="/clients/connect">Connect a client</Link>}
        >
          A service comes online when a client runs <code>burrow connect</code> for it.
        </EmptyState>
      ) : !live && (data ?? []).length === 0 ? (
        <EmptyState title="No services yet">
          Run <code>burrow connect</code> with <code>--type http</code> to expose a service.
        </EmptyState>
      ) : onlyGateway ? (
        <EmptyState
          title="All services here belong to the AI gateway."
          action={<Button variant="primary" size="sm" onClick={() => setAi(true)}>Show gateway-only services</Button>}
        >
          They have no address of their own, so they are left out until the AI filter is on.
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
              {filtered.map(({ key, name, type, state, service: s, tunnel: t }) => {
                const serviceId = s?.id ?? t?.service_id;
                const access = s?.access_mode ?? t?.access_mode ?? "open";
                const holder = t ? holders.get(t.id) : undefined;
                const target = s ?? (t && type === "http" && t.service_id
                  ? { id: t.service_id, name, type, access_mode: access }
                  : undefined);
                return (
              <tr
                key={key}
                // The name below stays the link, and the one stop for the keyboard: the row adds a bigger target only.
                className={serviceId ? "clickable" : undefined}
                onClick={serviceId ? (e) => openRow(e, serviceId) : undefined}
              >
                <td className="col-name link-row">
                  {serviceId ? <Link to={`/services/${serviceId}`}>{name}</Link> : name || "—"}
                </td>
                <td><Badge kind={`type-${type}`} nodot>{type}</Badge></td>
                <td>
                  {type === "tcp" && s
                    ? <TcpAddress service={s} host={relayHost} showPort={!t} />
                    : type !== "http"
                    ? <span className="muted">—</span>
                    : s?.gateway_only
                      ? (
                        <span className="row row-center gap-2">
                          <Badge kind="status-idle" nodot>gateway only</Badge>
                          {s.provider_slug && (
                            <Link className="link-inline" to={`/gateway/providers/${encodeURIComponent(s.provider_slug)}`}>
                              Provider: {s.provider_slug}
                            </Link>
                          )}
                        </span>
                      )
                      : <ServiceUrl slug={s?.slug ?? ""} url={s?.url || t?.url} />}
                </td>
                <td><Badge kind={`access-${access}`} nodot>{ACCESS_LABEL[access]}</Badge></td>
                {t && (
                  <>
                    <td>
                      {holder ? <Link to={`/clients/${holder.sessionId}`}>{holder.name}</Link> : <span className="muted">—</span>}
                    </td>
                    <td className="col-local">{t.local_addr}</td>
                    <td className="col-remote">
                      {type === "http" || !t.remote_port ? <span className="muted">—</span> : (
                        <span className="row row-center gap-2">
                          <span className="mono">:{t.remote_port}</span>
                          <button
                            type="button"
                            className="icon-btn"
                            aria-label={`Copy address ${relayHost}:${t.remote_port}`}
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
                    <td className="col-traffic">
                      <span title={`In: ${t.bytes_in} bytes`}>↓ {formatBytes(t.bytes_in)}</span>
                      {"  "}
                      <span title={`Out: ${t.bytes_out} bytes`}>↑ {formatBytes(t.bytes_out)}</span>
                    </td>
                  </>
                )}
                <td>
                  {state !== "online" || live
                    ? <StatusBadge state={state} />
                    // The AI filter goes along: without it the Online view would leave out the row just clicked.
                    : <Link to={ai ? "/services?live=1&ai=1" : "/services?live=1"} aria-label={`Online: show ${name} in the Online view`}><StatusBadge state={state} /></Link>}
                </td>
                <td className="col-actions">
                  {s?.type === "tcp"
                    ? <Link className="btn btn-secondary btn-sm" to={`/services/${encodeURIComponent(s.id)}`}>Configure</Link>
                    : target && <Button variant="secondary" size="sm" onClick={() => setConfigure(target)}>Configure</Button>}
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
        open={newOpen && canCreate}
        onOpenChange={(o) => { if (!o) closeNew(); }}
        title="New service"
        description="Reserve a service and its address before a client serves it."
        footer={
          <>
            <SubmitHint id="ns-hint" missing={nsMissing} />
            <Button variant="secondary" onClick={closeNew}>Cancel</Button>
            <Button
              variant="primary"
              disabled={nsHint !== undefined || createService.isPending}
              aria-describedby={nsHint !== undefined ? "ns-hint" : undefined}
              onClick={() => { clearNsErrors(); createService.mutate(); }}
            >
              {createService.isPending ? "Creating…" : "Create"}
            </Button>
          </>
        }
      >
        <FormFieldGroup>
          <FormField
            label="Name"
            htmlFor="ns-name"
            w="md"
            descId="ns-name-desc"
            error={nsNameError ?? undefined}
            help={nsNameMissing || nsHint !== undefined ? "The address and the service ID follow from it." : (
              <>
                {nsUrlSlug && <><span className="mono">{serviceUrl(nsUrlSlug, undefined, servicesDomain)}</span>{" · "}</>}
                ID <span className="mono">{nsId}</span>
              </>
            )}
          >
            <Input
              id="ns-name"
              value={nsName}
              maxLength={120}
              autoComplete="off"
              invalid={nsNameError !== null}
              aria-describedby="ns-name-desc"
              onChange={(e) => { setNsName(e.target.value); clearNsErrors(); }}
              onBlur={() => setNsNameLeft(true)}
            />
          </FormField>
          <p className="muted small">Apps that load assets from absolute paths (/assets/…) need base-path support to work under a /svc/ URL.</p>
          <FormField label="Access mode" htmlFor="ns-access-mode" w="md">
            <Select
              id="ns-access-mode"
              options={ACCESS_MODE_OPTIONS}
              value={nsAccessMode}
              onChange={setNsAccessMode}
            />
          </FormField>
        </FormFieldGroup>
        {/* Held open by a field in it: the toggle must not arm a close for the moment the value turns valid. */}
        <Disclosure label="Advanced" open={nsAdvancedOpen} onOpenChange={(o) => setNsAdvanced(o || nsAdvancedForced)}>
          <FormFieldGroup>
            <FormField
              label="Service ID"
              htmlFor="ns-service-id"
              w="md"
              descId="ns-service-id-desc"
              error={nsIdError ?? undefined}
              help={SERVICE_ID_HINT}
            >
              <Input
                id="ns-service-id"
                mono
                value={nsServiceId ?? derivedId}
                maxLength={64}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                invalid={nsIdError !== null}
                aria-describedby="ns-service-id-desc"
                onChange={(e) => { setNsServiceId(e.target.value.toLowerCase()); clearNsErrors(); }}
                onBlur={() => { setNsIdLeft(true); if (nsServiceId === "") setNsServiceId(null); }}
              />
            </FormField>
            <SlugField
              id="ns-slug"
              value={nsSlug ?? followedSlug}
              error={nsSlugErr}
              errorAfterBlur
              onChange={(v) => { setNsSlug(v); clearNsErrors(); }}
              onBlur={() => { if (nsSlug === "") setNsSlug(null); }}
            />
            <FormField label="Title" htmlFor="ns-title" w="md" descId="ns-title-desc" help="Shown in lists in place of the ID.">
              <Input
                id="ns-title"
                value={nsTitle ?? nsName}
                maxLength={120}
                autoComplete="off"
                aria-describedby="ns-title-desc"
                onChange={(e) => { setNsTitle(e.target.value); setNsErr(null); }}
                onBlur={() => { if (nsTitle?.trim() === "") setNsTitle(null); }}
              />
            </FormField>
          </FormFieldGroup>
        </Disclosure>
        {nsErr && <ErrorNotice>{nsErr}</ErrorNotice>}
      </Dialog>
      <Toaster />
    </div>
  );
}

/** The address of a tcp service: the command that reaches a private one, else host:port. */
function TcpAddress({ service, host, showPort }: { service: Service; host: string; showPort: boolean }) {
  if (service.access_mode === "private") {
    const cmd = forwardCommand(service.name || service.id);
    return <CopyAddress text={cmd} label={`Copy command ${cmd}`} />;
  }
  // The live view has the port in its own column.
  if (!showPort || !service.remote_port) return <span className="muted">—</span>;
  const addr = `${host}:${service.remote_port}`;
  return <CopyAddress text={addr} label={`Copy address ${addr}`} />;
}

function CopyAddress({ text, label }: { text: string; label: string }) {
  return (
    <span className="row row-center gap-2">
      <span className="mono">{text}</span>
      <button
        type="button"
        className="icon-btn"
        aria-label={label}
        onClick={() => { void navigator.clipboard?.writeText(text); toast.success("Copied."); }}
      >
        <Copy size={13} />
      </button>
    </span>
  );
}
