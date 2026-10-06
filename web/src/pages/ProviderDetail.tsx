import { useEffect, useId, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, DropdownMenu, ErrorNotice, FormField, Input, MetricStrip, MetricTile, PageHeader, Select, SkeletonRows, Switch, TableEmptyRow, Tabs } from "@/components/ds";
import { ProviderConnect } from "@/components/ProviderConnect";
import { RenameProviderDialog } from "@/components/RenameProviderDialog";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";
import { ProviderModelsPanel } from "@/components/ProviderModelsPanel";
import { ProviderConcurrencySetting, ProviderResponsesSetting, ProviderUpstreamPanel } from "@/components/ProviderUpstreamPanel";
import { useAuth } from "@/auth/useAuth";
import { providerBaseUrl } from "@/lib/serviceUrl";
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

function Sparkline({ data }: { data: number[] }) {
  const max = Math.max(1, ...data);
  const step = data.length > 1 ? 240 / (data.length - 1) : 0;
  const points = data.map((v, i) => `${(i * step).toFixed(2)},${(60 - (v / max) * 56 - 2).toFixed(2)}`).join(" ");
  return (
    <svg
      viewBox="0 0 240 60"
      role="img"
      aria-label="requests per minute, last hour"
      width="240"
      height="60"
    >
      <polyline points={points} fill="none" stroke="currentColor" strokeWidth="1.5" />
    </svg>
  );
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
const SUBTITLE = "Connection details, traffic, API keys, models and routing for this provider.";
// Tab values double as URL fragments, e.g. /gateway/providers/<slug>#api-keys.
const TAB_VALUES = ["connect", "api-keys", "models", "routing", "upstream"];

export default function ProviderDetail() {
  const { slug = "" } = useParams<{ slug: string }>();
  const nav = useNavigate();
  const qc = useQueryClient();
  const headingId = useId();
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const [renameOpen, setRenameOpen] = useState(false);
  // The open tab lives in the URL fragment, so it can be linked to and the
  // back button returns to it.
  const fragment = useLocation().hash.slice(1);
  const tab = TAB_VALUES.includes(fragment) ? fragment : "connect";
  const setTab = (next: string) => nav({ hash: `#${next}` }, { replace: true });

  const provider = useQuery({
    queryKey: ["ai", "provider", slug],
    queryFn: () => apiFetch<AiProvider>(`/ai/providers/${slug}`),
    retry: false,
    enabled: Boolean(slug),
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
    enabled: Boolean(id),
  });
  const metrics = useQuery({
    queryKey: ["ai", "provider", slug, "metrics"],
    queryFn: () => apiFetch<ProviderMetrics>(`/ai/providers/${slug}/metrics`),
    retry: false,
    enabled: Boolean(slug),
  });
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

  const aiRow = provider.data;
  // The first model that targets this provider, as the relay reports it. A
  // provider no model targets has no such line; " → " alone says nothing.
  const resolvedAlias = aiRow && (aiRow.model_alias || aiRow.concrete_model)
    ? `${aiRow.model_alias} → ${aiRow.concrete_model}`
    : null;

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

  if (!draft || !metrics.data || !provider.data || (!direct && !svc.data)) {
    return (
      <div className="ai-endpoint-detail-page">
        <PageHeader back={BACK} title="Provider" subtitle={SUBTITLE} />
        <SkeletonRows n={6} />
      </div>
    );
  }

  const routing = draft.routing;
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

  return (
    <div className="ai-endpoint-detail-page">
      <PageHeader
        back={BACK}
        title={`Provider · ${provider.data.name}`}
        subtitle={SUBTITLE}
        actions={
          <>
            {isAdmin && (
              <Button variant="secondary" size="sm" onClick={() => setRenameOpen(true)}>
                Rename
              </Button>
            )}
            <Link className="btn btn-secondary btn-sm" to={`/gateway/requests/${id}`}>
              Open request inspector
            </Link>
            {!direct && (
              <label className="row row-center gap-2">
                <span>Pause provider</span>
                <Switch
                  aria-label="Pause provider"
                  checked={routing.paused}
                  onChange={togglePause}
                />
              </label>
            )}
            <DropdownMenu
              trigger={
                <button type="button" className="icon-btn" aria-label="More actions">
                  <MoreHorizontal size={14} aria-hidden="true" />
                </button>
              }
              items={[
                { label: "Rotate cache", onSelect: () => clearCache.mutate() },
                { label: "Clear cache", onSelect: () => clearCache.mutate() },
                { label: "Export logs (NDJSON)", onSelect: () => { void apiFetch(`/services/${id}/inspector/export?format=ndjson`); } },
                ...(isAdmin
                  ? [{ label: "Delete provider", danger: true, onSelect: () => { setDeleteErr(null); setDeleteOpen(true); } }]
                  : []),
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
        // "#upstream" on a tunnel provider names a tab it does not have.
        value={tab === "upstream" && !direct ? "connect" : tab}
        onChange={setTab}
        tabs={[
          {
            value: "connect",
            label: "Connect",
            content: (
              <>
              <ProviderConnect
                baseUrl={providerBaseUrl(provider.data.slug, provider.data.base_url)}
                exampleModel={models.data?.[0]?.id || provider.data.concrete_model || undefined}
              />

              {direct ? (
                resolvedAlias && (
                  <div className="meta-strip">
                    <span className="mono">{resolvedAlias}</span>
                  </div>
                )
              ) : (
                <div className="meta-strip">
                  {resolvedAlias && <span className="mono">{resolvedAlias}</span>}
                  {aiRow?.client_session_id && (
                    <Link to={`/clients/${aiRow.client_session_id}`} className="mono">
                      {aiRow.client_session_id}
                    </Link>
                  )}
                  <span className="muted">
                    last seen {aiRow?.status === "Connected" ? "just now" : "—"}
                  </span>
                </div>
              )}

              {flat && (
                <p className="muted small">Flat-rate plan: usage is counted in tokens, not in USD.</p>
              )}

              {/* A direct provider has this among its upstream settings. */}
              {!direct && provider.data.api_format === "openai" && (
                <ProviderResponsesSetting provider={provider.data} isAdmin={isAdmin} />
              )}
              {!direct && (
                // Keyed on the stored limit: a save or a refetch that brings
                // another value starts the field from it.
                <ProviderConcurrencySetting
                  key={provider.data.max_concurrent}
                  provider={provider.data}
                  isAdmin={isAdmin}
                />
              )}

              <MetricStrip ariaLabel="Provider metrics">
                <MetricTile label="Requests (24h)" value={fmtInt(metrics.data.requests_24h)} />
                <MetricTile label="Tokens (24h)" value={`${fmtInt(metrics.data.tokens_in_24h)} → ${fmtInt(metrics.data.tokens_out_24h)}`} />
                <MetricTile label="Cost (24h)" value={flat ? "—" : `$${metrics.data.cost_usd_24h.toFixed(2)}`} />
                <MetricTile label="Cache hit ratio" value={`${Math.round(metrics.data.cache_hit_ratio_24h * 100)}%`} />
              </MetricStrip>

              <div className="sparkline-wrap">
                <Sparkline data={metrics.data.requests_per_minute} />
              </div>

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
                        <th>Latency</th>
                        <th>Cache</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(recent.data ?? []).map((r) => (
                        <tr
                          key={r.id}
                          role="button"
                          tabIndex={0}
                          onClick={() => nav(`/gateway/requests/${id}/${r.id}`)}
                          onKeyDown={(e) => {
                            if (e.key !== "Enter" && e.key !== " ") return;
                            e.preventDefault();
                            nav(`/gateway/requests/${id}/${r.id}`);
                          }}
                          className="clickable"
                        >
                          <td className="mono small">{r.ts}</td>
                          <td className="mono">{r.method}</td>
                          <td className="mono">{r.path}</td>
                          <td className="mono">{r.status}</td>
                          <td className="mono">{r.duration_ms} ms</td>
                          <td className="mono">{r.cache}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </section>
              </>
            ),
          },
          {
            value: "api-keys",
            label: "API keys",
            content: <ApiKeysPanel serviceId={id} ownToaster={false} />,
          },
          {
            value: "models",
            label: "Models",
            content: <ProviderModelsPanel slug={provider.data.slug} kind={provider.data.kind} isAdmin={isAdmin} />,
          },
          {
            value: "routing",
            label: "Routing",
            content: (
              <>
              <section aria-labelledby={`${headingId}-routing`} className="card">
                <h2 id={`${headingId}-routing`}>Routing</h2>
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
                  <Button variant="primary" size="sm" disabled={save.isPending} onClick={() => save.mutate(draft)}>
                    {save.isPending ? "Saving…" : "Save routing"}
                  </Button>
                </div>
              </section>

              <section aria-labelledby={`${headingId}-backends`} className="card">
                <h2 id={`${headingId}-backends`}>Backends</h2>
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
            ),
          },
          ...(direct
            ? [{
                value: "upstream",
                label: "Upstream",
                content: <ProviderUpstreamPanel provider={provider.data} isAdmin={isAdmin} />,
              }]
            : []),
        ]}
      />

      {isAdmin && (
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
            Clients that use this base URL will start receiving errors.{" "}
            {direct
              ? `Its API keys, its AI configuration and its model list are deleted with it. The credential slot ${provider.data.credential_slot} on the relay is not touched.`
              : "The service, its API keys and its tunnel are kept."}
          </p>
          {deleteErr && <ErrorNotice>{deleteErr}</ErrorNotice>}
        </Dialog>
      )}

      {isAdmin && (
        <RenameProviderDialog
          provider={provider.data}
          open={renameOpen}
          onOpenChange={setRenameOpen}
          onRenamed={(next) => {
            if (next.slug === slug) return;
            // Same backing service, so the numbers carry over: no skeleton, and
            // the Rename button keeps the focus the dialog handed back.
            qc.setQueryData(["ai", "provider", next.slug, "metrics"], metrics.data);
            nav(`/gateway/providers/${next.slug}#${tab}`, { replace: true });
          }}
        />
      )}

      {/* The one toaster of the page; the API keys panel is told not to mount its own. */}
      <Toaster />
    </div>
  );
}
