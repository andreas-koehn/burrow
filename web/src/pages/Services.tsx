import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp } from "lucide-react";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Badge, Dialog, EmptyState, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, Select, SkeletonRows } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import type { Service, AccessMode } from "@/lib/contract";
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

export default function Services() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const [configure, setConfigure] = useState<Service | null>(null);
  const panelRef = useRef<AccessModePanelHandle>(null);

  // P2-2 — filter + sort for the Services table. Default sort: type asc,
  // name asc, matching Tunnels for muscle-memory parity.
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
  const filtered = useMemo(() => {
    const list = data ?? [];
    const f = q
      ? list.filter((s) =>
          `${s.name} ${s.type} ${s.slug ?? ""} ${s.url ?? ""}`.toLowerCase().includes(q.toLowerCase()))
      : list;
    const sgn = sortDir === "asc" ? 1 : -1;
    return [...f].sort((a, b) => {
      let cmp = 0;
      if (sortKey === "name") cmp = a.name.localeCompare(b.name);
      else if (sortKey === "type") {
        cmp = a.type.localeCompare(b.type);
        if (cmp === 0) cmp = a.name.localeCompare(b.name);
      } else {
        cmp = Number(b.connected) - Number(a.connected);
        if (cmp === 0) cmp = a.name.localeCompare(b.name);
      }
      return cmp * sgn;
    });
  }, [data, q, sortKey, sortDir]);

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
      setSearchParams({}, { replace: true });
    } else if (newParam === "1") {
      setNewOpen(true);
      setSearchParams({}, { replace: true });
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
    onSuccess: (resp) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      if (aiFlow && resp?.id) {
        closeNew();
        nav(`/services/${resp.id}#upstream-key`);
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

  return (
    <div className="services-page">
      <PageHeader
        title="Services"
        subtitle="Durable services exposed through this relay, with their access configuration."
        actions={<Button variant="primary" size="sm" onClick={() => { setNewOpen(true); setNsErr(null); }}>New service</Button>}
      />
      <ErrorNotice variant="info" role="note">
        Services are the durable saved config and access mode. When a client is connected, the
        live link appears in <Link to="/tunnels">Tunnels</Link>.
      </ErrorNotice>

      {error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void refetch()}>Retry</Button>}
        >
          Couldn't load services: {error instanceof ApiError ? error.message : "Unknown error"}
        </ErrorNotice>
      ) : isLoading ? (
        <div className="table-wrap skel-pad">
          <SkeletonRows n={4} />
        </div>
      ) : !data || data.length === 0 ? (
        <EmptyState title="No services yet">
          Run <code>burrow connect</code> with <code>--type http</code> to expose a service.
        </EmptyState>
      ) : (
        <>
          <div className="toolbar-row">
            <Input
              type="search"
              aria-label="Filter services"
              placeholder="filter by name, type, URL…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <div className="table-wrap">
            <table className="data" aria-label="Services">
              <thead>
                <tr>
                  <th>
                    <button type="button" className="sort-header" onClick={() => toggleSort("name")}
                      aria-label={`Sort by name (${sortKey === "name" ? sortDir : "asc"})`}>
                      Name {sortIcon("name")}
                    </button>
                  </th>
                  <th>
                    <button type="button" className="sort-header" onClick={() => toggleSort("type")}
                      aria-label={`Sort by type (${sortKey === "type" ? sortDir : "asc"})`}>
                      Type {sortIcon("type")}
                    </button>
                  </th>
                  <th>URL</th>
                  <th>Access</th>
                  <th>
                    <button type="button" className="sort-header" onClick={() => toggleSort("status")}
                      aria-label={`Sort by status (${sortKey === "status" ? sortDir : "asc"})`}>
                      Status {sortIcon("status")}
                    </button>
                  </th>
                  <th className="col-actions"></th>
                </tr>
              </thead>
              <tbody>
                {filtered.map((s) => (
                <tr key={s.id}>
                  <td className="col-name link-row">
                    <Link to={`/services/${s.id}`}>{s.name}</Link>
                  </td>
                  <td><Badge kind={`type-${s.type}`} nodot>{s.type}</Badge></td>
                  <td>
                    {s.type === "http" ? <ServiceUrl slug={s.slug} url={s.url} /> : <span className="muted">—</span>}
                  </td>
                  <td><Badge kind={`access-${s.access_mode}`} nodot>{ACCESS_LABEL[s.access_mode]}</Badge></td>
                  <td>
                    {s.connected
                      ? <Link to="/tunnels" aria-label={`View live tunnel for ${s.name}`}><Badge kind="status-connected">connected</Badge></Link>
                      : <Badge kind="status-idle">idle</Badge>}
                  </td>
                  <td className="col-actions">
                    <Button variant="secondary" size="sm" onClick={() => setConfigure(s)}>Configure</Button>
                  </td>
                </tr>
              ))}
              </tbody>
            </table>
          </div>
        </>
      )}

      <Dialog
        open={configure !== null}
        onOpenChange={(o) => { if (!o) setConfigure(null); }}
        size="lg"
        title={configure ? `Access · ${configure.name}` : ""}
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
            serviceName={configure.name}
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
          ? "Creates a service with API-key access. Next you'll bind an upstream key so it appears under AI endpoints."
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
