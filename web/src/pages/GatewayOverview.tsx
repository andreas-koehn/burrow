import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Sparkles } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { Badge, Button, EmptyState, ErrorNotice, MetricStrip, MetricTile, PageHeader, SkeletonRows } from "@/components/ds";
import { ProviderConnect } from "@/components/ProviderConnect";
import { SetupChecklist, type ChecklistStep } from "@/components/SetupChecklist";
import { useAuth } from "@/auth/useAuth";
import { apiFetch, ApiError } from "@/lib/api";
import { providerBaseUrl } from "@/lib/serviceUrl";
import { statusLabel } from "@/lib/status";
import { useRelayNotices } from "@/lib/useRelayNotices";
import type { AiProvider, Budget, CostSummary } from "@/lib/contract";

/** Share of its daily limit from which a budget is worth a notice. */
const BUDGET_NEAR = 0.8;

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
}

/** Whether requests can reach the provider, in the words of the Providers page where it has them. */
function reachability(p: AiProvider): { badge: string; text: string } {
  // A hosted API has no client to be connected: it is ready once its credential is set.
  if (p.kind === "direct") {
    return p.credential_present
      ? { badge: "status-connected", text: "ready" }
      : { badge: "status-idle", text: "not configured" };
  }
  if (p.status === "Offline") return { badge: "status-offline", text: "client offline" };
  return { badge: p.status === "Degraded" ? "status-degraded" : "status-connected", text: statusLabel(p.status) };
}

function budgetNotices(budgets: Budget[]): { id: string; variant: "error" | "warn"; message: string }[] {
  const over = budgets.filter((b) => b.exceeded).length;
  const near = budgets.filter((b) => !b.exceeded && b.daily_usd > 0 && b.current_usd / b.daily_usd > BUDGET_NEAR).length;
  const out: { id: string; variant: "error" | "warn"; message: string }[] = [];
  if (over > 0) {
    out.push({ id: "over", variant: "error", message: over === 1 ? "A budget is over its daily limit." : `${over} budgets are over their daily limit.` });
  }
  if (near > 0) {
    out.push({ id: "near", variant: "warn", message: near === 1 ? "A budget is above 80 % of its daily limit." : `${near} budgets are above 80 % of their daily limit.` });
  }
  return out;
}

export default function GatewayOverview() {
  const { user, loading: authLoading } = useAuth();
  const isAdmin = user?.role === "admin";
  const relayNotices = useRelayNotices();

  // Same keys and fetches as the Providers and the Cost & budgets pages.
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const cost = useQuery({
    queryKey: ["cost", "summary", "today"],
    queryFn: () => apiFetch<CostSummary>("/cost/summary?window=today"),
    retry: false,
    staleTime: 60_000,
  });
  const budgets = useQuery({
    queryKey: ["budgets"],
    queryFn: () => apiFetch<Budget[]>("/budgets"),
    enabled: isAdmin,
    retry: false,
  });

  const header = <PageHeader title="AI Gateway" subtitle="Model providers, policy, requests and cost." />;

  // A relay built without the AI gateway answers 404; zeros would suggest an empty but working one.
  if (providers.error instanceof ApiError && providers.error.status === 404) {
    return (
      <div className="gateway-overview-page">
        {header}
        <EmptyState icon={<Sparkles size={18} />} title="AI gateway isn't available on this relay">
          Ask your operator to enable it, or run a build that includes the AI gateway.
        </EmptyState>
      </div>
    );
  }

  const list = Array.isArray(providers.data) ? providers.data : [];
  const first = list[0];
  const totalRequests = list.reduce((a, p) => a + p.requests_24h, 0);
  const costAbsent = cost.error instanceof ApiError && cost.error.status === 404;
  const notices = budgetNotices(isAdmin && Array.isArray(budgets.data) ? budgets.data : []);

  // requests_24h alone would reopen the last step after a quiet day; tokens or
  // cost counted in the current window are proof of a request as well.
  const served = totalRequests > 0
    || (cost.data ? cost.data.total_usd > 0 || cost.data.tokens_in > 0 || cost.data.tokens_out > 0 : false);

  const steps: ChecklistStep[] = [
    // Only an admin can add a provider.
    ...(isAdmin ? [{
      id: "provider",
      title: "Add a provider",
      description: "A provider is a model backend under its own base URL: a service with API-key access, or a hosted API the relay calls itself.",
      done: list.length > 0,
      action: { label: "Add a provider", to: "/gateway/providers" },
    }] : []),
    {
      id: "key",
      title: "Create an API key",
      description: "Clients sign in to a provider with one of its API keys.",
      done: list.some((p) => p.api_key_count > 0),
      action: { label: "Create an API key", to: first ? `/gateway/providers/${first.slug}` : "/gateway/providers" },
    },
    {
      id: "request",
      title: "Send the first request",
      description: "Point any OpenAI-compatible client at the provider's base URL.",
      done: served,
      // Without a provider there is nothing to connect to yet.
      ...(first
        ? { content: <ProviderConnect baseUrl={providerBaseUrl(first.slug, first.base_url)} exampleModel={first.concrete_model} /> }
        : { action: { label: "Add a provider", to: "/gateway/providers" } }),
    },
  ];

  return (
    <div className="gateway-overview-page">
      {header}

      {providers.error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void providers.refetch()}>Retry</Button>}
        >
          Couldn't load providers:{" "}
          {providers.error instanceof ApiError ? providers.error.message : "Unknown error"}
        </ErrorNotice>
      ) : providers.isLoading || authLoading ? (
        <SkeletonRows n={3} />
      ) : (
        <>
          {/* No error-rate and no latency tile: the relay reports neither per provider. */}
          <MetricStrip ariaLabel="AI Gateway">
            <MetricTile label="Requests 24h" value={fmtInt(totalRequests)} sub="across all providers" to="/gateway/requests" />
            {!costAbsent && (
              <MetricTile
                label="Cost 24h"
                value={cost.data ? fmtUsd(cost.data.total_usd) : "—"}
                sub="estimate"
                tooltip="Estimates from the bundled pricing table — operator-overridable."
                to="/gateway/cost"
              />
            )}
          </MetricStrip>

          {(relayNotices.length > 0 || notices.length > 0) && (
            <div className="alerts-strip">
              {relayNotices.map((n) => (
                <ErrorNotice key={n.id} variant="warn" role="status" action={<Link to={n.action.to}>{n.action.label} →</Link>}>
                  {n.message}
                </ErrorNotice>
              ))}
              {notices.map((n) => (
                <ErrorNotice key={n.id} variant={n.variant} role="status" action={<Link to="/gateway/cost">View budgets →</Link>}>
                  {n.message}
                </ErrorNotice>
              ))}
            </div>
          )}

          {list.length > 0 && (
            <div className="table-wrap">
              <table className="data" aria-label="Providers">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Status</th>
                    <th>Requests (24h)</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((p) => {
                    const reach = reachability(p);
                    return (
                      <tr key={p.slug}>
                        <td className="col-name"><Link to={`/gateway/providers/${p.slug}`}>{p.name}</Link></td>
                        <td><Badge kind={reach.badge}>{reach.text}</Badge></td>
                        <td className="mono">{fmtInt(p.requests_24h)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          {list.length === 0 && !isAdmin ? (
            <EmptyState icon={<Sparkles size={18} />} title="No providers yet">
              An administrator can add one from a service in API-key mode, or add a hosted API.
            </EmptyState>
          ) : (
            // The last step reads the cost summary, so the list waits for that answer.
            !cost.isPending && <SetupChecklist title="Set up the AI Gateway" steps={steps} />
          )}
        </>
      )}
      <Toaster />
    </div>
  );
}
