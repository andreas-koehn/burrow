import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, MoreHorizontal, Sparkles } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { statusLabel } from "@/lib/status";
import { Badge, Button, DropdownMenu, EmptyState, ErrorNotice, MetricStrip, MetricTile, PageHeader, SkeletonRows } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { NewProviderDialog } from "@/components/NewProviderDialog";
import { providerBaseUrl } from "@/lib/serviceUrl";
import type { AiProvider, CostSummary } from "@/lib/contract";

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
}

function hitRatio(hits: number, requests: number): string {
  if (requests <= 0) return "0%";
  return `${Math.round((hits / requests) * 100)}%`;
}


const STATUS_BADGE: Record<AiProvider["status"], string> = {
  Connected: "status-connected",
  Degraded: "status-degraded",
  Offline: "status-offline",
};

export default function Providers() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const [newOpen, setNewOpen] = useState(false);
  const endpoints = useQuery({
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

  const list = endpoints.data ?? [];
  const hasEndpoints = list.length > 0;
  const totalRequests = list.reduce((a, e) => a + e.requests_24h, 0);
  const totalCacheHits = list.reduce((a, e) => a + e.cache_hits_24h, 0);
  const tokensIn = summary.data?.tokens_in ?? 0;
  const tokensOut = summary.data?.tokens_out ?? 0;
  const totalUsd = summary.data?.total_usd ?? 0;

  // P1-9 — when the AI gateway feature isn't compiled into this relay the
  // endpoint returns 404; rendering the KPI strip with zeros next to the
  // error banner suggests an empty-but-working install. Detect that case
  // and show a single "feature unavailable" card instead.
  const featureAbsent = endpoints.error instanceof ApiError && endpoints.error.status === 404;

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
          <>
            <Button variant="secondary" size="sm" onClick={() => nav("/services?new=ai")}>
              New AI service
            </Button>
            <Button variant="primary" size="sm" onClick={() => setNewOpen(true)}>
              New provider
            </Button>
          </>
        ) : undefined}
      />
      <p className="muted small" style={{ marginBottom: "var(--space-3, 12px)" }}>
        A provider serves a <Link to="/services">Service</Link> with API-key access and an OpenAI-compatible upstream.
      </p>

      <MetricStrip ariaLabel="Provider metrics">
        <MetricTile label="Requests (24h)" value={fmtInt(totalRequests)} />
        <MetricTile
          label="Tokens in/out (24h)"
          value={hasEndpoints ? `${fmtInt(tokensIn)} → ${fmtInt(tokensOut)}` : "—"}
        />
        <MetricTile
          label="Cost estimate (24h)"
          value={hasEndpoints ? fmtUsd(totalUsd) : "—"}
          tooltip="Estimates from the bundled pricing table — operator-overridable."
        />
        <MetricTile
          label="Cache hit ratio (24h)"
          value={hitRatio(totalCacheHits, totalRequests)}
          sub={`${fmtInt(totalCacheHits)} / ${fmtInt(totalRequests)}`}
        />
      </MetricStrip>

      {endpoints.error ? (
        <ErrorNotice
          action={
            <Button variant="secondary" size="sm" onClick={() => void endpoints.refetch()}>
              Retry
            </Button>
          }
        >
          Couldn't load providers:{" "}
          {endpoints.error instanceof ApiError ? endpoints.error.message : "Unknown error"}
        </ErrorNotice>
      ) : endpoints.isLoading ? (
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
            ? "Add one from a service in API-key mode."
            : "An administrator can add one from a service in API-key mode."}
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Providers">
            <thead>
              <tr>
                <th>Name</th>
                <th>Base URL</th>
                <th>Backend</th>
                <th>Keys</th>
                <th>Requests (24h)</th>
                <th>Cache hits</th>
                <th>Latency p95</th>
                <th>Status</th>
                <th className="col-actions"></th>
              </tr>
            </thead>
            <tbody>
              {list.map((e) => {
                const baseUrl = providerBaseUrl(e.slug, e.base_url);
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
                  <td>
                    <span className="row row-center gap-2 service-url">
                      <span className="mono service-url-path" title={baseUrl}>{`/ai/${e.slug}/v1`}</span>
                      <button
                        type="button"
                        className="icon-btn"
                        aria-label={`Copy base URL ${baseUrl}`}
                        onClick={() => void navigator.clipboard?.writeText(baseUrl)}
                      >
                        <Copy size={13} />
                      </button>
                    </span>
                  </td>
                  <td>
                    <Badge kind={`backend-${e.backend_type}`} nodot>
                      {e.backend_type}
                    </Badge>
                  </td>
                  <td className="mono">{fmtInt(e.api_key_count)}</td>
                  <td className="mono">{fmtInt(e.requests_24h)}</td>
                  <td>
                    <span className="mono">{fmtInt(e.cache_hits_24h)}</span>
                    <span className="muted small">
                      {" "}
                      ({hitRatio(e.cache_hits_24h, e.requests_24h)})
                    </span>
                  </td>
                  <td className="mono">{fmtInt(e.latency_p95_ms)} ms</td>
                  <td>
                    <Badge kind={STATUS_BADGE[e.status]}>{statusLabel(e.status)}</Badge>
                  </td>
                  <td className="col-actions">
                    <DropdownMenu
                      trigger={
                        <button
                          type="button"
                          className="icon-btn"
                          aria-label={`More actions for ${e.name}`}
                        >
                          <MoreHorizontal size={14} />
                        </button>
                      }
                      items={[
                        { label: "Inspect", onSelect: () => nav(`/gateway/providers/${e.slug}`) },
                        { label: "Keys", onSelect: () => nav(`/services?focus=${e.service_id}&panel=api-keys`) },
                        { label: "Access settings", onSelect: () => nav(`/services?focus=${e.service_id}`) },
                        { label: "Cost", onSelect: () => nav(`/cost`) },
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
      {isAdmin && <NewProviderDialog open={newOpen} onOpenChange={setNewOpen} />}
    </div>
  );
}
