import { useEffect, useId, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, DropdownMenu, ErrorNotice, FormField, FormFieldGroup, Input, MetricStrip, MetricTile, PageHeader, Select, SkeletonRows, Switch, TableEmptyRow } from "@/components/ds";
import { ProviderConnect } from "@/components/ProviderConnect";
import { RenameProviderDialog } from "@/components/RenameProviderDialog";
import { useAuth } from "@/auth/useAuth";
import { providerBaseUrl } from "@/lib/serviceUrl";
import type {
  AiProvider, ModelAliasV5, Provider, Service, ServiceAIConfig,
} from "@/lib/contract";
import { withAIConfigDefaults } from "@/lib/aiConfig";

interface EndpointMetrics {
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
      aria-label="requests per minute, last 24h"
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

const PROVIDER_OPTIONS: { value: Provider; label: string }[] = [
  { value: "ollama", label: "Ollama" },
  { value: "vllm", label: "vLLM" },
  { value: "openai-compat", label: "OpenAI-compat" },
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic" },
  { value: "other", label: "Other" },
];

interface AliasFormState {
  alias: string;
  concrete_model: string;
  service_id: string;
  provider: Provider;
  priority: number;
}


const BACK = { to: "/gateway/providers", label: "Providers" } as const;
const SUBTITLE = "Routing, traffic, and recent traffic for this provider.";

export default function ProviderDetail() {
  const { slug = "" } = useParams<{ slug: string }>();
  const nav = useNavigate();
  const qc = useQueryClient();
  const headingId = useId();
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const [renameOpen, setRenameOpen] = useState(false);

  const provider = useQuery({
    queryKey: ["ai", "provider", slug],
    queryFn: () => apiFetch<AiProvider>(`/ai/providers/${slug}`),
    retry: false,
    enabled: Boolean(slug),
  });
  // Everything below is per service; the provider names the service.
  const id = provider.data?.service_id ?? "";

  const svc = useQuery({
    queryKey: ["service", id],
    queryFn: () => apiFetch<Service>(`/services/${id}`),
    retry: false,
    enabled: Boolean(id),
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
    queryFn: () => apiFetch<EndpointMetrics>(`/ai/providers/${slug}/metrics`),
    retry: false,
    enabled: Boolean(slug),
  });
  const aliases = useQuery({
    queryKey: ["models", "aliases"],
    queryFn: () => apiFetch<ModelAliasV5[]>("/models/aliases"),
    retry: false,
  });
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
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

  // Add alias dialog state
  const [aliasDialogOpen, setAliasDialogOpen] = useState(false);
  const [aliasForm, setAliasForm] = useState<AliasFormState>({
    alias: "",
    concrete_model: "",
    service_id: id,
    provider: "ollama",
    priority: 100,
  });

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

  const disable = useMutation({
    mutationFn: () => apiFetch<void>(`/services/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Provider disabled.");
      qc.invalidateQueries({ queryKey: ["services"] });
      qc.invalidateQueries({ queryKey: ["ai", "providers"] });
      nav("/gateway/providers");
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't disable provider."),
  });

  const createAlias = useMutation({
    mutationFn: (data: AliasFormState) =>
      apiFetch<ModelAliasV5>("/models/aliases", {
        method: "POST",
        body: JSON.stringify(data),
      }),
    onSuccess: () => {
      toast.success("Alias created.");
      qc.invalidateQueries({ queryKey: ["models", "aliases"] });
      setAliasDialogOpen(false);
      setAliasForm({ alias: "", concrete_model: "", service_id: id, provider: "ollama", priority: 100 });
    },
    onError: (e: unknown) => {
      toast.error(e instanceof ApiError ? e.message : "Couldn't create alias.");
    },
  });

  const updatePriority = useMutation({
    mutationFn: ({ alias, priority }: { alias: string; priority: number }) =>
      apiFetch<void>(`/models/aliases/${alias}`, {
        method: "PUT",
        body: JSON.stringify({ priority }),
      }),
    onSuccess: () => {
      toast.success("Priority updated.");
      qc.invalidateQueries({ queryKey: ["models", "aliases"] });
    },
    onError: (e: unknown) => {
      toast.error(e instanceof ApiError ? e.message : "Couldn't update priority.");
    },
  });

  const aiRow = provider.data;
  const alias = (aliases.data ?? []).find((a) => a.service_id === id);
  const resolvedAlias =
    alias && draft?.routing.model_alias
      ? `${draft.routing.model_alias} → ${alias.concrete_model}`
      : aiRow
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

  if (!draft || !metrics.data || !svc.data || !provider.data) {
    return (
      <div className="ai-endpoint-detail-page">
        <PageHeader back={BACK} title="Provider" subtitle={SUBTITLE} />
        <SkeletonRows n={6} />
      </div>
    );
  }

  const routing = draft.routing;

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

  // Look up provider chip for a backend row
  function getProviderForBackend(service_id: string): string {
    const match = (aliases.data ?? []).find((a) => a.service_id === service_id);
    return match?.provider ?? "—";
  }

  // Service options for the "Add alias" dialog
  const serviceOptions = (services.data ?? []).map((s) => ({ value: s.id, label: s.name }));

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
            <label className="row row-center gap-2">
              <span>Pause provider</span>
              <Switch
                aria-label="Pause provider"
                checked={routing.paused}
                onChange={togglePause}
              />
            </label>
            <DropdownMenu
              trigger={
                <button type="button" className="icon-btn" aria-label="More actions">
                  <MoreHorizontal size={14} />
                </button>
              }
              items={[
                { label: "Rotate cache", onSelect: () => clearCache.mutate() },
                { label: "Clear cache", onSelect: () => clearCache.mutate() },
                { label: "Disable", danger: true, onSelect: () => disable.mutate() },
                { label: "Export logs (NDJSON)", onSelect: () => { void apiFetch(`/services/${id}/inspector/export?format=ndjson`); } },
              ]}
            />
          </>
        }
      />

      <ProviderConnect
        baseUrl={providerBaseUrl(provider.data.slug, provider.data.base_url)}
        exampleModel={provider.data.concrete_model || undefined}
      />

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

      <MetricStrip ariaLabel="Provider metrics">
        <MetricTile label="Requests (24h)" value={fmtInt(metrics.data.requests_24h)} />
        <MetricTile label="Tokens (24h)" value={`${fmtInt(metrics.data.tokens_in_24h)} → ${fmtInt(metrics.data.tokens_out_24h)}`} />
        <MetricTile label="Cost (24h)" value={`$${metrics.data.cost_usd_24h.toFixed(2)}`} />
        <MetricTile label="Cache hit ratio" value={`${Math.round(metrics.data.cache_hit_ratio_24h * 100)}%`} />
      </MetricStrip>

      <div className="sparkline-wrap">
        <Sparkline data={metrics.data.requests_per_minute} />
      </div>

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
        <div className="panel-head">
          <h2 id={`${headingId}-backends`}>Backends</h2>
          <Button
            variant="primary"
            size="sm"
            onClick={() => {
              setAliasForm({ alias: "", concrete_model: "", service_id: id, provider: "ollama", priority: 100 });
              setAliasDialogOpen(true);
            }}
          >
            Add alias
          </Button>
        </div>
        <div className="table-wrap">
          <table className="data" aria-label="Backends">
            <thead>
              <tr>
                <th>Service</th>
                <th>Concrete model</th>
                <th>Weight</th>
                <th>Provider</th>
                <th>Priority</th>
              </tr>
            </thead>
            <tbody>
              {(routing.backends ?? []).map((b) => (
                <tr key={b.service_id}>
                  <td className="mono">{b.service_id}</td>
                  <td className="mono">{b.concrete_model}</td>
                  <td className="mono">{b.weight}</td>
                  <td>
                    <span className="chip">
                      {getProviderForBackend(b.service_id)}
                    </span>
                  </td>
                  <td>
                    {(() => {
                      const matched = (aliases.data ?? []).find((a) => a.service_id === b.service_id);
                      return (
                        <Input
                          type="number"
                          min={0}
                          max={999}
                          className="mono"
                          aria-label={`Priority for ${matched?.alias ?? b.service_id}`}
                          defaultValue={String(matched?.priority ?? 100)}
                          disabled={!matched}
                          onBlur={(e) => {
                            if (matched) {
                              updatePriority.mutate({ alias: matched.alias, priority: Number(e.target.value) });
                            }
                          }}
                        />
                      );
                    })()}
                  </td>
                </tr>
              ))}
              {(routing.backends ?? []).length === 0 && (
                <TableEmptyRow colSpan={5} title="No backends configured.">Add an alias to get started.</TableEmptyRow>
              )}
            </tbody>
          </table>
        </div>
      </section>

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
                  onClick={() => nav(`/inspector/${id}/${r.id}`)}
                  onKeyDown={(e) => { if (e.key === "Enter") nav(`/inspector/${id}/${r.id}`); }}
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

      {/* Add alias dialog */}
      <Dialog
        open={aliasDialogOpen}
        onOpenChange={setAliasDialogOpen}
        title="Add alias"
        description="Create a new model alias binding for this provider."
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setAliasDialogOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={createAlias.isPending}
              onClick={() => createAlias.mutate(aliasForm)}
            >
              {createAlias.isPending ? "Creating…" : "Create alias"}
            </Button>
          </>
        }
      >
        <FormFieldGroup>
          <FormField label="Alias" htmlFor="alias-field-alias" w="md">
            <Input
              id="alias-field-alias"
              value={aliasForm.alias}
              onChange={(e) => setAliasForm((f) => ({ ...f, alias: e.target.value }))}
              placeholder="e.g. fast"
            />
          </FormField>
          <FormField label="Concrete model" htmlFor="alias-field-model" w="md">
            <Input
              id="alias-field-model"
              value={aliasForm.concrete_model}
              onChange={(e) => setAliasForm((f) => ({ ...f, concrete_model: e.target.value }))}
              placeholder="e.g. llama3.1:8b"
            />
          </FormField>
          <FormField label="Service" htmlFor="alias-field-service" w="md">
            <Select
              id="alias-field-service"
              value={aliasForm.service_id}
              onChange={(v) => setAliasForm((f) => ({ ...f, service_id: v }))}
              options={serviceOptions.length > 0 ? serviceOptions : [{ value: id, label: id }]}
            />
          </FormField>
          <FormField label="Provider" htmlFor="alias-field-provider" w="md">
            <Select
              id="alias-field-provider"
              value={aliasForm.provider}
              onChange={(v) => setAliasForm((f) => ({ ...f, provider: v as Provider }))}
              options={PROVIDER_OPTIONS}
            />
          </FormField>
          <FormField label="Priority" htmlFor="alias-field-priority" w="sm">
            <Input
              id="alias-field-priority"
              type="number"
              min={0}
              max={999}
              value={String(aliasForm.priority)}
              onChange={(e) => setAliasForm((f) => ({ ...f, priority: Number(e.target.value) }))}
            />
          </FormField>
        </FormFieldGroup>
      </Dialog>

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
            nav(`/gateway/providers/${next.slug}`, { replace: true });
          }}
        />
      )}

      <Toaster />
    </div>
  );
}
