import { useCallback, useEffect, useId, useRef, useState, type MouseEvent } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, MoreHorizontal } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, DropdownMenu, ErrorNotice, FormField, Input, MetricStrip, MetricTile, PageHeader, Select, SkeletonRows, Switch, TableEmptyRow, Tabs } from "@/components/ds";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";
import { ProviderClientPanel } from "@/components/ProviderClientPanel";
import { ProviderConnect } from "@/components/ProviderConnect";
import { RenameProviderDialog } from "@/components/RenameProviderDialog";
import { ProviderModelsPanel } from "@/components/ProviderModelsPanel";
import { ProviderConcurrencySetting, ProviderResponsesSetting, ProviderUpstreamPanel, SlotsNotSet } from "@/components/ProviderUpstreamPanel";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { providerBaseUrl } from "@/lib/serviceUrl";
import { providerState } from "@/lib/status";
import { formatRatio } from "@/lib/format";
import { StatusBadge } from "@/components/StatusBadge";
import { RelativeTime } from "@/components/RelativeTime";
import type {
  AiProvider, AiProviderModel, Service, ServiceAIConfig,
} from "@/lib/contract";
import { withAIConfigDefaults } from "@/lib/aiConfig";

interface ProviderMetrics {
  requests_24h: number;
  tokens_in_24h: number;
  tokens_out_24h: number;
  cost_usd_24h: number;
  cache_hit_ratio_24h: number;
  requests_per_minute: number[];
}

interface InspectorRow {
  id: string;
  ts: string;
  method: string;
  path: string;
  status: number;
  duration_ms: number;
  cache: "HIT" | "MISS" | "SKIP";
}

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

const STRATEGY_OPTIONS = [
  { value: "single", label: "Single backend" },
  { value: "failover", label: "Failover" },
  { value: "weighted", label: "Weighted" },
  { value: "header_based", label: "Header-based" },
  { value: "sticky", label: "Sticky session" },
  { value: "multi_provider", label: "Multi-provider (cross-backend)" },
];

const BACK = { to: "/gateway/providers", label: "Providers" } as const;
// How often a provider a client serves is asked again, so the page says Offline by itself
// when the client stops. While none serves it, the client panel asks (WaitForProvider).
const ONLINE_POLL_MS = 3000;
const SUBTITLE = "Status, use in apps, catalog and settings of this provider.";
// Tab values double as URL fragments, e.g. /gateway/providers/<slug>#settings.
const TAB_VALUES = ["overview", "catalog", "settings"];
// Fragments of the tabs this page had before: links and bookmarks keep working.
const OLD_FRAGMENT = new Map([
  ["connect", "overview"],
  ["models", "catalog"],
  ["api-keys", "settings"],
  ["routing", "settings"],
  ["upstream", "settings"],
]);

export default function ProviderDetail() {
  const { slug = "" } = useParams<{ slug: string }>();
  const nav = useNavigate();
  const qc = useQueryClient();
  const headingId = useId();
  // Every write on this page is a provider write; the clients list is read by whoever sees every client.
  const canManage = useCan(P.aiProvidersManage);
  const canListClients = useCan(P.tunnelsReadAny);
  // GET and PUT /services/{id}/ai-config: ai:configure:own or :any. Routing, backends and Pause live there.
  const canConfigureAi = useCan([P.aiConfigureOwn, P.aiConfigureAny]);
  const [renameOpen, setRenameOpen] = useState(false);
  // The open tab lives in the URL fragment, so it can be linked to and the
  // back button returns to it.
  const fragment = useLocation().hash.slice(1);
  const tab = TAB_VALUES.includes(fragment) ? fragment : OLD_FRAGMENT.get(fragment) ?? "overview";
  const setTab = (next: string) => nav({ hash: `#${next}` }, { replace: true });
  // The routing settings are collapsed until asked for; "#routing" asks for them.
  const [advancedChoice, setAdvancedChoice] = useState<boolean | null>(null);
  const advancedOpen = advancedChoice ?? fragment === "routing";

  const provider = useQuery({
    queryKey: ["ai", "provider", slug],
    queryFn: () => apiFetch<AiProvider>(`/ai/providers/${slug}`),
    retry: false,
    enabled: Boolean(slug),
    refetchInterval: (q) => {
      const p = q.state.data;
      return q.state.status !== "error" && p?.kind === "tunnel" && providerState(p) === "online" ? ONLINE_POLL_MS : false;
    },
  });
  // Everything below is per service; the provider names the service.
  const id = provider.data?.service_id ?? "";
  // A direct provider has no tunnel and no client; its backing service is not
  // a service anyone manages, so the page never asks for it.
  const direct = provider.data?.kind === "direct";

  const svc = useQuery({
    queryKey: ["service", id],
    queryFn: () => apiFetch<Service>(`/services/${id}`),
    retry: false,
    enabled: Boolean(id) && !direct,
  });
  const cfg = useQuery({
    queryKey: ["service", id, "ai-config"],
    queryFn: async () => {
      const c = await apiFetch<Partial<ServiceAIConfig>>(`/services/${id}/ai-config`);
      // Merge over defaults: the GET can return {} (unconfigured) or a partial
      // blob, and the page reads nested fields (routing/cache/inspector/etc.).
      return withAIConfigDefaults(c);
    },
    retry: false,
    enabled: Boolean(id) && canConfigureAi,
  });
  const metrics = useQuery({
    queryKey: ["ai", "provider", slug, "metrics"],
    queryFn: () => apiFetch<ProviderMetrics>(`/ai/providers/${slug}/metrics`),
    retry: false,
    enabled: Boolean(slug),
  });
  // The client panel waited for a client and one came: read again what it changes.
  const cameOnline = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["ai", "provider", slug], exact: true });
    void qc.invalidateQueries({ queryKey: ["service", id], exact: true });
  }, [qc, slug, id]);
  // The client stopped: what the backing service says about it is read again too.
  const wasOnline = useRef(false);
  const onlineNow = provider.data !== undefined && providerState(provider.data) === "online";
  useEffect(() => {
    if (wasOnline.current && !onlineNow && id) void qc.invalidateQueries({ queryKey: ["service", id], exact: true });
    wasOnline.current = onlineNow;
  }, [onlineNow, id, qc]);
  // A service key was revoked on this page: the provider counts them.
  const keyRevoked = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["ai", "provider", slug], exact: true });
    void qc.invalidateQueries({ queryKey: ["ai", "providers"] });
  }, [qc, slug]);
  // Same key as ProviderModelsPanel; the first model is used in the examples.
  const models = useQuery({
    queryKey: ["ai", "provider-models", slug],
    queryFn: () => apiFetch<AiProviderModel[]>(`/ai/providers/${slug}/models`),
    retry: false,
    enabled: Boolean(slug),
  });
  const recent = useQuery({
    queryKey: ["inspector", id],
    queryFn: () => apiFetch<InspectorRow[]>(`/services/${id}/inspector/requests?limit=10`),
    retry: false,
    enabled: Boolean(id),
  });

  // A recent request opens on a click anywhere in its row, as a service does on the Services list.
  function openRequest(e: MouseEvent<HTMLTableRowElement>, requestId: string) {
    if ((e.target as Element).closest("a")) return;
    // A click with a key held asks for a new tab or window, which only the path's link can open.
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    // Marking a path to copy it ends in a click too.
    if (window.getSelection()?.toString()) return;
    nav(`/gateway/requests/${id}/${requestId}`);
  }

  // Local draft of the routing tab — synced from cfg.data on first arrival.
  const [draft, setDraft] = useState<ServiceAIConfig | null>(null);
  useEffect(() => {
    if (cfg.data && !draft) setDraft(cfg.data);
  }, [cfg.data, draft]);

  const save = useMutation({
    mutationFn: (next: ServiceAIConfig) =>
      apiFetch<void>(`/services/${id}/ai-config`, {
        method: "PUT",
        body: JSON.stringify(next),
      }),
    onSuccess: () => {
      toast.success("Routing saved.");
      qc.invalidateQueries({ queryKey: ["service", id, "ai-config"] });
    },
    onError: (e: unknown) => {
      toast.error(e instanceof ApiError ? e.message : "Couldn't save routing.");
    },
  });

  const clearCache = useMutation({
    mutationFn: () =>
      apiFetch<void>(`/services/${id}/cache/entries`, { method: "DELETE" }),
    onSuccess: () => toast.success("Cache cleared."),
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't clear cache."),
  });

  // Deleting a tunnel provider removes only the provider; a direct provider
  // takes its backing service, API keys and model list with it.
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const remove = useMutation({
    mutationFn: () => apiFetch<void>(`/ai/providers/${slug}`, { method: "DELETE" }),
    onSuccess: async () => {
      // Leave, and leave this provider's cache entries alone: removing them
      // while the page is still mounted would refetch what was just deleted.
      nav("/gateway/providers");
      await qc.invalidateQueries({ queryKey: ["ai", "providers"] });
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setDeleteErr("Couldn't delete the provider.");
      else if (e.status === 403) setDeleteErr("You don't have permission to delete providers.");
      else setDeleteErr(e.message);
    },
  });

  if (provider.error || svc.error || cfg.error || metrics.error) {
    const e = provider.error ?? svc.error ?? cfg.error ?? metrics.error;
    return (
      <div className="ai-endpoint-detail-page">
        <PageHeader
          back={BACK}
          title="Provider"
          subtitle={SUBTITLE}
        />
        <ErrorNotice
          action={
            <Button variant="secondary" size="sm" onClick={() => { void provider.refetch(); void svc.refetch(); void cfg.refetch(); void metrics.refetch(); }}>
              Retry
            </Button>
          }
        >
          Couldn't load provider: {e instanceof ApiError ? e.message : "Unknown error"}
        </ErrorNotice>
      </div>
    );
  }

  // Without the permission the config is not read; the page shows what it can without it.
  const view = draft ?? (canConfigureAi ? null : withAIConfigDefaults({}));
  if (!view || !metrics.data || !provider.data || (!direct && !svc.data)) {
    return (
      <div className="ai-endpoint-detail-page">
        <PageHeader back={BACK} title="Provider" subtitle={SUBTITLE} />
        <SkeletonRows n={6} />
      </div>
    );
  }

  const routing = view.routing;
  const flat = direct && provider.data.billing === "flat";

  function setRouting(next: Partial<ServiceAIConfig["routing"]>) {
    if (!draft) return;
    setDraft({ ...draft, routing: { ...draft.routing, ...next } });
  }
  function setCircuitBreaker(next: Partial<ServiceAIConfig["routing"]["circuit_breaker"]>) {
    if (!draft) return;
    setDraft({
      ...draft,
      routing: { ...draft.routing, circuit_breaker: { ...draft.routing.circuit_breaker, ...next } },
    });
  }

  const togglePause = () => {
    if (!draft) return;
    const next: ServiceAIConfig = {
      ...draft,
      routing: { ...draft.routing, paused: !draft.routing.paused },
    };
    setDraft(next);
    save.mutate(next);
  };

  const sticky = routing.strategy === "sticky";
  const state = providerState(provider.data);
  // Keys of the backing service, from before gateway keys: they still authenticate.
  const serviceKeys = provider.data.api_key_count ?? 0;
  const missingSlots = provider.data.credential_slots?.filter((x) => !x.present).map((x) => x.slot) ?? [];

  return (
    <div className="ai-endpoint-detail-page">
      <PageHeader
        back={BACK}
        title={`Provider · ${provider.data.name}`}
        subtitle={SUBTITLE}
        actions={
          <>
            <StatusBadge state={state} />
            <Link className="btn btn-secondary btn-sm" to={`/gateway/requests?provider=${encodeURIComponent(provider.data.slug)}`}>
              View requests
            </Link>
            <DropdownMenu
              trigger={
                <button type="button" className="icon-btn" aria-label="More actions">
                  <MoreHorizontal size={14} aria-hidden="true" />
                </button>
              }
              items={[
                { label: "Clear cache", onSelect: () => clearCache.mutate() },
                { label: "Export logs (NDJSON)", onSelect: () => { void apiFetch(`/services/${id}/inspector/export?format=ndjson`); } },
              ]}
            />
          </>
        }
      />

      {provider.data.breaker_open && (
        <ErrorNotice variant="info" role="note">
          Burrow is skipping this provider for now because recent requests failed. It will be tried again automatically.
        </ErrorNotice>
      )}

      <Tabs
        value={tab}
        onChange={setTab}
        tabs={[
          {
            value: "overview",
            label: "Overview",
            content: (
              <>
              {direct ? (
                <section aria-labelledby={`${headingId}-credential`} className="card col gap-2">
                  <h2 id={`${headingId}-credential`}>Credential</h2>
                  <p>
                    <StatusBadge state={state} />{" · "}
                    {provider.data.credential_present
                      ? "The credential is set on the relay."
                      : <SlotsNotSet slots={missingSlots.length > 0 ? missingSlots : [provider.data.credential_slot]} />}
                  </p>
                  {!provider.data.credential_present && (
                    <p className="small">
                      <Link className="link-inline" to={{ hash: "#settings" }} replace>Upstream settings</Link>
                    </p>
                  )}
                </section>
              ) : svc.data && (
                <ProviderClientPanel provider={provider.data} service={svc.data} canListClients={canListClients} onOnline={cameOnline} />
              )}

              <ProviderConnect
                baseUrl={providerBaseUrl(provider.data.slug, provider.data.base_url)}
                exampleModel={models.data?.[0]?.id || provider.data.concrete_model || undefined}
                apiFormat={provider.data.api_format}
                provider={provider.data.slug}
              />

              {flat && (
                <p className="muted small">Flat-rate plan: usage is counted in tokens, not in USD.</p>
              )}

              <MetricStrip ariaLabel="Provider metrics">
                <MetricTile label="Requests (24h)" value={fmtInt(metrics.data.requests_24h)} />
                <MetricTile label="Tokens (24h)" value={`${fmtInt(metrics.data.tokens_in_24h)} → ${fmtInt(metrics.data.tokens_out_24h)}`} />
                <MetricTile label="Cost (24h)" value={flat ? "—" : `$${metrics.data.cost_usd_24h.toFixed(2)}`} />
                {/* The relay sends the ratio; without a request there is none to state. */}
                <MetricTile label="Cache hit ratio" value={formatRatio(metrics.data.cache_hit_ratio_24h * metrics.data.requests_24h, metrics.data.requests_24h)} />
              </MetricStrip>

              <section aria-labelledby={`${headingId}-recent`} className="card">
                <h2 id={`${headingId}-recent`}>Recent requests</h2>
                <div className="table-wrap">
                  <table className="data" aria-label="Recent requests">
                    <thead>
                      <tr>
                        <th>When</th>
                        <th>Method</th>
                        <th>Path</th>
                        <th>Status</th>
                        {/* No Latency, as on the Requests list: the relay sets no duration on a captured request. */}
                        <th>Cache</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(recent.data ?? []).map((r) => (
                        // The path stays the link, and the one stop for the keyboard: the row adds a bigger target only.
                        <tr key={r.id} className="clickable" onClick={(e) => openRequest(e, r.id)}>
                          <td><RelativeTime value={r.ts} /></td>
                          <td className="mono">{r.method}</td>
                          <td><Link className="mono" to={`/gateway/requests/${id}/${r.id}`}>{r.path}</Link></td>
                          <td className="mono">{r.status}</td>
                          <td className="mono">{r.cache}</td>
                        </tr>
                      ))}
                      {/* Only once the answer is there: an empty table is not yet a table without requests. */}
                      {recent.data && recent.data.length === 0 && (
                        <TableEmptyRow colSpan={5} title="No requests yet" />
                      )}
                    </tbody>
                  </table>
                </div>
              </section>
              </>
            ),
          },
          {
            value: "catalog",
            label: "Catalog",
            content: <ProviderModelsPanel slug={provider.data.slug} kind={provider.data.kind} canEdit={canManage} />,
          },
          {
            value: "settings",
            label: "Settings",
            content: (
              <>
              {/* A direct provider has these two among its upstream settings. */}
              {!direct && provider.data.api_format === "openai" && (
                <ProviderResponsesSetting provider={provider.data} canEdit={canManage} />
              )}
              {!direct && (
                // Keyed on the stored limit: a save or a refetch that brings
                // another value starts the field from it.
                <ProviderConcurrencySetting
                  key={provider.data.max_concurrent}
                  provider={provider.data}
                  canEdit={canManage}
                />
              )}
              {direct && <ProviderUpstreamPanel provider={provider.data} canEdit={canManage} />}

              <section aria-labelledby={`${headingId}-keys`} className="card col gap-2">
                <h2 id={`${headingId}-keys`}>Keys</h2>
                <p>
                  Apps use a gateway key. <Link className="link-inline" to="/gateway/keys">Manage gateway keys</Link>
                </p>
                {serviceKeys > 0 && !direct && (
                  <p className="muted small">
                    This provider's service also has {serviceKeys === 1 ? "1 API key" : `${fmtInt(serviceKeys)} API keys`} from
                    before gateway keys. {serviceKeys === 1 ? "It keeps working." : "They keep working."}{" "}
                    <Link className="link-inline" to={`/services/${id}#api-keys`}>
                      {serviceKeys === 1 ? "Manage it" : "Manage them"}
                    </Link>
                  </p>
                )}
                {/* A hosted provider's backing service has no page of its own: its keys are
                    listed here, to revoke only. A new key for a model is a gateway key. */}
                {serviceKeys > 0 && direct && (
                  <>
                    <p className="muted small">
                      This provider also has {serviceKeys === 1 ? "1 API key" : `${fmtInt(serviceKeys)} API keys`} from
                      before gateway keys.{" "}
                      {serviceKeys === 1 ? "It keeps working until you revoke it." : "They keep working until you revoke them."}
                    </p>
                    <ApiKeysPanel serviceId={id} ownToaster={false} revokeOnly onRevoked={keyRevoked} />
                  </>
                )}
              </section>

              {/* Set apart like the other sections a page keeps closed (Connect a client). */}
              {canConfigureAi && <section aria-labelledby={`${headingId}-advanced`} className="account-section">
                <h2 id={`${headingId}-advanced`} className="connect-other-head">
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-expanded={advancedOpen}
                    aria-controls={`${headingId}-advanced-panel`}
                    icon={advancedOpen ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
                    onClick={() => setAdvancedChoice(!advancedOpen)}
                  >
                    Advanced
                  </Button>
                </h2>
                <div id={`${headingId}-advanced-panel`} hidden={!advancedOpen}>
                  {advancedOpen && (
                    <>
                    <p className="muted small">
                      Routing applies to requests on this provider's own address when backends are configured.
                      Where a model's requests go is set by its targets.
                    </p>
                    <section aria-labelledby={`${headingId}-routing`} className="card">
                      <h3 id={`${headingId}-routing`}>Routing</h3>
                      <div className="form-grid">
                        <div className="field">
                          <label htmlFor={`${headingId}-strategy`}>Routing strategy</label>
                          <Select
                            id={`${headingId}-strategy`}
                            value={routing.strategy}
                            onChange={(v) =>
                              setRouting({ strategy: v as ServiceAIConfig["routing"]["strategy"] })
                            }
                            options={STRATEGY_OPTIONS}
                          />
                        </div>
                        {routing.strategy === "multi_provider" && (
                          <p
                            data-testid="multi-provider-banner"
                            className="notice-inline info"
                            style={{ gridColumn: "1 / -1" }}
                          >
                            Cross-provider failover is allowed only when <code>Idempotency-Key</code> is set and zero bytes have streamed. See routing docs.
                          </p>
                        )}
                        <div className="field">
                          <label className="row gap-2" htmlFor={`${headingId}-sticky`}>
                            <Switch
                              id={`${headingId}-sticky`}
                              aria-label="Sticky session"
                              checked={sticky}
                              onChange={(v) => setRouting({ strategy: v ? "sticky" : "single" })}
                            />
                            <span>Sticky session</span>
                          </label>
                        </div>
                        <FormField label="Circuit-breaker failure %" htmlFor={`${headingId}-fpct`} w="sm">
                          <Input
                            id={`${headingId}-fpct`}
                            type="number"
                            className="mono"
                            min={0}
                            max={100}
                            value={routing.circuit_breaker.failure_pct}
                            onChange={(e) => setCircuitBreaker({ failure_pct: Number(e.target.value) })}
                          />
                        </FormField>
                      </div>
                      <div className="actions">
                        <Button variant="primary" size="sm" disabled={save.isPending} onClick={() => save.mutate(view)}>
                          {save.isPending ? "Saving…" : "Save routing"}
                        </Button>
                      </div>
                    </section>

                    <section aria-labelledby={`${headingId}-backends`} className="card">
                      <h3 id={`${headingId}-backends`}>Backends</h3>
                      <div className="table-wrap">
                        <table className="data" aria-label="Backends">
                          <thead>
                            <tr>
                              <th>Service</th>
                              <th>Concrete model</th>
                              <th>Weight</th>
                            </tr>
                          </thead>
                          <tbody>
                            {(routing.backends ?? []).map((b) => (
                              <tr key={b.service_id}>
                                {/* A direct provider's backing service is not a service anyone manages: no id. */}
                                {direct && b.service_id === id
                                  ? <td>This provider</td>
                                  : <td className="mono">{b.service_id}</td>}
                                <td className="mono">{b.concrete_model}</td>
                                <td className="mono">{b.weight}</td>
                              </tr>
                            ))}
                            {(routing.backends ?? []).length === 0 && (
                              <TableEmptyRow colSpan={3} title="No backends configured." />
                            )}
                          </tbody>
                        </table>
                      </div>
                    </section>
                    </>
                  )}
                </div>
              </section>}

              {/* A hosted provider is not paused, and only whoever may manage providers renames or deletes: nothing is left for anyone else. */}
              {((!direct && canConfigureAi) || canManage) && (
                <section aria-labelledby={`${headingId}-manage`} className="card col gap-2">
                  <h2 id={`${headingId}-manage`}>This provider</h2>
                  {!direct && canConfigureAi && (
                    <label className="row row-center gap-2">
                      <Switch
                        aria-label="Pause provider"
                        checked={routing.paused}
                        onChange={togglePause}
                      />
                      <span>Pause provider</span>
                    </label>
                  )}
                  {canManage && (
                    <div className="row row-center gap-2">
                      <Button variant="secondary" size="sm" onClick={() => setRenameOpen(true)}>
                        Rename
                      </Button>
                      <Button variant="destructive" size="sm" onClick={() => { setDeleteErr(null); setDeleteOpen(true); }}>
                        Delete provider
                      </Button>
                    </div>
                  )}
                </section>
              )}
              </>
            ),
          },
        ]}
      />

      {canManage && (
        <Dialog
          open={deleteOpen}
          onOpenChange={(o) => { if (!remove.isPending) setDeleteOpen(o); }}
          title={`Delete provider · ${provider.data.name}`}
          description={`${providerBaseUrl(provider.data.slug, provider.data.base_url)} stops working at once. This cannot be undone.`}
          footer={
            <>
              <Button variant="secondary" disabled={remove.isPending} onClick={() => setDeleteOpen(false)}>Cancel</Button>
              <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {remove.isPending ? "Deleting…" : "Delete provider"}
              </Button>
            </>
          }
        >
          <p className="muted">
            Apps that use this base URL will start receiving errors.{" "}
            {direct
              ? `Its API keys, its AI configuration and its model list are deleted with it. The credential slot ${provider.data.credential_slot} on the relay is not touched.`
              : "The service, its API keys and its client connection are kept."}
          </p>
          {deleteErr && <ErrorNotice>{deleteErr}</ErrorNotice>}
        </Dialog>
      )}

      {canManage && (
        <RenameProviderDialog
          provider={provider.data}
          open={renameOpen}
          onOpenChange={setRenameOpen}
          onRenamed={(next) => {
            if (next.slug === slug) return;
            // Same backing service, so the numbers carry over: no skeleton, and
            // the Rename button on Settings keeps the focus the dialog handed back.
            qc.setQueryData(["ai", "provider", next.slug, "metrics"], metrics.data);
            nav(`/gateway/providers/${next.slug}#${tab}`, { replace: true });
          }}
        />
      )}

      {/* The one toaster of the page; the key list of a hosted provider is told not to mount its own. */}
      <Toaster />
    </div>
  );
}
