import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, MoreHorizontal, Sparkles } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { providerState } from "@/lib/status";
import { StatusBadge } from "@/components/StatusBadge";
import { Badge, Button, DropdownMenu, EmptyState, ErrorNotice, MetricStrip, MetricTile, PageHeader, SkeletonRows } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { NewProviderDialog } from "@/components/NewProviderDialog";
import { providerBaseUrl } from "@/lib/serviceUrl";
import { formatLatency, formatRatio } from "@/lib/format";
import { providerKindLabel } from "@/lib/providerPresets";
import type { AiProvider, CostSummary } from "@/lib/contract";

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
}


export default function Providers() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const { user, loading } = useAuth();
  const isAdmin = user?.role === "admin";
  const [newOpen, setNewOpen] = useState(false);
  // /gateway/providers?new=local opens the dialog too. The parameter stays
  // while the dialog is open, so a reload opens it again, and goes when it closes.
  const [searchParams, setSearchParams] = useSearchParams();
  const linked = searchParams.get("new") === "local";
  // The link is no use to someone who may not add a provider: the page says
  // who can, and keeps saying it once the parameter is taken out.
  const refused = linked && !loading && !isAdmin;
  const [needsAdmin, setNeedsAdmin] = useState(false);
  if (refused && !needsAdmin) setNeedsAdmin(true);
  useEffect(() => {
    if (!refused) return;
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.delete("new");
      return next;
    }, { replace: true });
  }, [refused, setSearchParams]);
  function setDialogOpen(open: boolean) {
    setNewOpen(open);
    if (!open && linked) {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        next.delete("new");
        return next;
      }, { replace: true });
    }
  }
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const summary = useQuery({
    queryKey: ["ai", "summary"],
    queryFn: () => apiFetch<CostSummary>("/cost/summary?window=today"),
    retry: false,
  });

  // Live updates: invalidate on the existing SSE "tunnels" channel.
  // jsdom test envs without an EventSource stub skip the subscription.
  useEffect(() => {
    if (typeof EventSource === "undefined") return;
    const es = new EventSource("/api/v1/events");
    const onTick = () => qc.invalidateQueries({ queryKey: ["ai", "providers"] });
    es.addEventListener("tunnels", onTick);
    return () => {
      es.removeEventListener("tunnels", onTick);
      es.close();
    };
  }, [qc]);

  const list = providers.data ?? [];
  const hasProviders = list.length > 0;
  const totalRequests = list.reduce((a, e) => a + e.requests_24h, 0);
  const totalCacheHits = list.reduce((a, e) => a + e.cache_hits_24h, 0);
  const tokensIn = summary.data?.tokens_in ?? 0;
  const tokensOut = summary.data?.tokens_out ?? 0;
  const totalUsd = summary.data?.total_usd ?? 0;

  // P1-9 — when the AI gateway feature isn't compiled into this relay the
  // endpoint returns 404; rendering the KPI strip with zeros next to the
  // error banner suggests an empty-but-working install. Detect that case
  // and show a single "feature unavailable" card instead.
  const featureAbsent = providers.error instanceof ApiError && providers.error.status === 404;

  if (featureAbsent) {
    return (
      <div className="ai-endpoints-page">
        <PageHeader title="Providers" />
        <EmptyState
          icon={<Sparkles size={18} />}
          title="AI gateway isn't available on this relay"
        >
          Ask your operator to enable it, or run a build that includes the AI gateway.
        </EmptyState>
      </div>
    );
  }

  return (
    <div className="ai-endpoints-page">
      <PageHeader
        title="Providers"
        subtitle="Model backends served through this relay, each under its own base URL — with cache, cost, and traffic at a glance."
        actions={isAdmin ? (
          <Button variant="primary" size="sm" onClick={() => setNewOpen(true)}>
            New provider
          </Button>
        ) : undefined}
      />
      <p className="muted small" style={{ marginBottom: "var(--space-3, 12px)" }}>
        A provider serves a <Link to="/services">Service</Link> with API-key access and an OpenAI-compatible upstream,
        or a hosted API that the relay calls itself.
      </p>

      {needsAdmin && (
        <ErrorNotice variant="info" role="status">Adding a provider needs an admin.</ErrorNotice>
      )}

      <MetricStrip ariaLabel="Provider metrics">
        <MetricTile label="Requests (24h)" value={fmtInt(totalRequests)} />
        <MetricTile
          label="Tokens in/out (24h)"
          value={hasProviders ? `${fmtInt(tokensIn)} → ${fmtInt(tokensOut)}` : "—"}
        />
        <MetricTile
          label="Cost estimate (24h)"
          value={hasProviders ? fmtUsd(totalUsd) : "—"}
          tooltip="Estimates from the bundled pricing table — operator-overridable."
        />
        <MetricTile
          label="Cache hit ratio (24h)"
          value={formatRatio(totalCacheHits, totalRequests)}
          sub={`${fmtInt(totalCacheHits)} / ${fmtInt(totalRequests)}`}
        />
      </MetricStrip>

      {providers.error ? (
        <ErrorNotice
          action={
            <Button variant="secondary" size="sm" onClick={() => void providers.refetch()}>
              Retry
            </Button>
          }
        >
          Couldn't load providers:{" "}
          {providers.error instanceof ApiError ? providers.error.message : "Unknown error"}
        </ErrorNotice>
      ) : providers.isLoading ? (
        <div className="table-wrap skel-pad">
          <SkeletonRows n={3} />
        </div>
      ) : list.length === 0 ? (
        <EmptyState
          icon={<Sparkles size={18} />}
          title="No providers yet"
          action={isAdmin ? (
            <Button variant="primary" size="sm" onClick={() => setNewOpen(true)}>
              New provider
            </Button>
          ) : undefined}
        >
          {isAdmin
            ? "Add a model server on one of your machines, or a hosted API."
            : "An admin can add a model server on one of their machines, or a hosted API."}
          {" Switching a service to API-key mode does not create a provider."}
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Providers">
            <thead>
              <tr>
                <th>Name</th>
                <th>Kind</th>
                <th>Base URL</th>
                <th>Requests (24h)</th>
                <th>Latency p95</th>
                <th>Status</th>
                <th className="col-actions" aria-label="Actions"></th>
              </tr>
            </thead>
            <tbody>
              {list.map((e) => {
                const baseUrl = providerBaseUrl(e.slug, e.base_url);
                const direct = e.kind === "direct";
                return (
                <tr key={e.slug}>
                  <td className="col-name">
                    <div><Link to={`/gateway/providers/${e.slug}`}>{e.name}</Link></div>
                    {(e.model_alias || e.concrete_model) && (
                      <div className="mono muted small">
                        {`${e.model_alias} → ${e.concrete_model}`}
                      </div>
                    )}
                  </td>
                  {/* Served by a client, or which hosted API: the one thing a "backend" beside it used to repeat. */}
                  <td><Badge kind={`type-${e.kind}`} nodot>{providerKindLabel(e)}</Badge></td>
                  <td>
                    <span className="row row-center gap-2 service-url">
                      {/* In full: what is shown is what the button copies. */}
                      <span className="mono service-url-path">{baseUrl}</span>
                      <button
                        type="button"
                        className="icon-btn"
                        aria-label={`Copy base URL ${baseUrl}`}
                        onClick={() => { void navigator.clipboard?.writeText(baseUrl); toast.success("Copied."); }}
                      >
                        <Copy size={13} aria-hidden="true" />
                      </button>
                    </span>
                  </td>
                  <td className="mono">{fmtInt(e.requests_24h)}</td>
                  {/* Without a request there is no latency to state. */}
                  <td className="mono">{formatLatency(e.latency_p95_ms, e.requests_24h)}</td>
                  <td>
                    <StatusBadge state={providerState(e)} />
                  </td>
                  <td className="col-actions">
                    <DropdownMenu
                      trigger={
                        <button
                          type="button"
                          className="icon-btn"
                          aria-label={`More actions for ${e.name}`}
                        >
                          <MoreHorizontal size={14} aria-hidden="true" />
                        </button>
                      }
                      // "Keys" are the API keys the backing service holds from before
                      // gateway keys; without any there is nothing to lead to. A direct
                      // provider's service has no page of its own: its keys are listed on
                      // the provider's Settings tab. A tunnel provider's are on its service.
                      items={direct ? [
                        { label: "Inspect", onSelect: () => nav(`/gateway/providers/${e.slug}`) },
                        ...(e.api_key_count > 0
                          ? [{ label: "Keys", onSelect: () => nav(`/gateway/providers/${e.slug}#settings`) }]
                          : []),
                        { label: "Cost", onSelect: () => nav(`/gateway/cost`) },
                      ] : [
                        { label: "Inspect", onSelect: () => nav(`/gateway/providers/${e.slug}`) },
                        ...(e.api_key_count > 0
                          ? [{ label: "Keys", onSelect: () => nav(`/services/${e.service_id}#api-keys`) }]
                          : []),
                        { label: "Access settings", onSelect: () => nav(`/services?focus=${e.service_id}`) },
                        { label: "Cost", onSelect: () => nav(`/gateway/cost`) },
                      ]}
                    />
                  </td>
                </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {isAdmin && <NewProviderDialog open={newOpen || linked} onOpenChange={setDialogOpen} />}
      <Toaster />
    </div>
  );
}
