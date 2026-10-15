import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Sparkles } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { Button, EmptyState, ErrorNotice, MetricStrip, MetricTile, PageHeader, SkeletonRows } from "@/components/ds";
import { ConnectCard } from "@/components/ConnectCard";
import { GatewayAddresses } from "@/components/GatewayAddresses";
import { SetupChecklist, type ChecklistStep } from "@/components/SetupChecklist";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { apiFetch, ApiError } from "@/lib/api";
import { providerState } from "@/lib/status";
import { StatusBadge } from "@/components/StatusBadge";
import { useRelayNotices } from "@/lib/useRelayNotices";
import type { AiGatewayKey, AiModel, AiProvider, Budget, CostSummary, GatewayInfo } from "@/lib/contract";

/** Share of its daily limit from which a budget is worth a notice. */
const BUDGET_NEAR = 0.8;

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
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
  const { loading: authLoading } = useAuth();
  // GET /budgets: budgets:manage. POST /ai/providers: ai:providers:manage. POST /ai/models: ai:configure:any.
  const canReadBudgets = useCan(P.budgetsManage);
  const canAddProvider = useCan(P.aiProvidersManage);
  const canCreateModel = useCan(P.aiConfigureAny);
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
  // Same keys and fetches as the Models and the Gateway keys pages.
  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
  });
  const keys = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
  });
  const gateway = useQuery({
    queryKey: ["ai", "gateway"],
    queryFn: () => apiFetch<GatewayInfo>("/ai/gateway"),
    retry: false,
  });
  const budgets = useQuery({
    queryKey: ["budgets"],
    queryFn: () => apiFetch<Budget[]>("/budgets"),
    enabled: canReadBudgets,
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
  const modelList = Array.isArray(models.data) ? models.data : [];
  const keyList = Array.isArray(keys.data) ? keys.data : [];
  const endpoints = gateway.data?.endpoints ?? [];
  const setupFailed = models.error ?? keys.error;
  const totalRequests = list.reduce((a, p) => a + p.requests_24h, 0);
  const costAbsent = cost.error instanceof ApiError && cost.error.status === 404;
  const notices = budgetNotices(canReadBudgets && Array.isArray(budgets.data) ? budgets.data : []);

  // requests_24h alone would reopen the last step after a quiet day; tokens or
  // cost counted in the current window are proof of a request as well.
  const served = totalRequests > 0
    || (cost.data ? cost.data.total_usd > 0 || cost.data.tokens_in > 0 || cost.data.tokens_out > 0 : false);

  const steps: ChecklistStep[] = [
    ...(canAddProvider ? [{
      id: "provider",
      title: "Add a provider",
      description: "A provider serves models under its own base URL: a model server on one of your machines, or a hosted API the relay calls itself.",
      done: list.length > 0,
      // Opens the guided flow, not just the page it is on.
      action: { label: "Add a provider", to: "/gateway/providers?new=local" },
    }] : []),
    ...(canCreateModel ? [{
      id: "model",
      title: "Create a model",
      description: "A model is a name apps ask for, and for each API format the provider it is sent to.",
      done: modelList.length > 0,
      action: { label: "Create a model", to: "/gateway/models" },
    }] : []),
    {
      id: "key",
      title: "Create a gateway key",
      description: "Apps sign in to the gateway with a gateway key.",
      // A reader without ai:read:any sees their own keys: the step is about a key they can use.
      done: keyList.some((k) => !k.revoked_at),
      action: { label: "Create a gateway key", to: "/gateway/keys" },
    },
    {
      id: "request",
      title: "Send the first request",
      description: "Point Claude Code, Codex or any OpenAI-compatible app at the gateway.",
      done: served,
      content: (
        <ConnectCard
          canCreate={canCreateModel}
          endpoints={endpoints}
          models={modelList.filter((m) => m.enabled)}
        />
      ),
    },
  ];

  return (
    <div className="gateway-overview-page">
      {/* Above the title: a notice is about the relay or the gateway, not about one of the page's parts. */}
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

      {header}
      {/* In full: on this page the addresses say where the gateway is, the job the sidebar's "/ai/…" had. */}
      <GatewayAddresses endpoints={endpoints} full />

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

          {/* Before the table while it has open steps: what is still to do comes first. */}
          {list.length === 0 && !canAddProvider ? (
            <EmptyState icon={<Sparkles size={18} />} title="No providers yet">
              An administrator can add a model server on one of their machines, or a hosted API.
            </EmptyState>
          ) : setupFailed ? (
            // Without the models or the keys the list would call existing ones missing.
            <ErrorNotice
              action={<Button variant="secondary" size="sm" onClick={() => { void models.refetch(); void keys.refetch(); }}>Retry</Button>}
            >
              {models.error ? "Couldn't load models: " : "Couldn't load gateway keys: "}
              {setupFailed instanceof ApiError ? setupFailed.message : "Unknown error"}
            </ErrorNotice>
          ) : (
            // The steps read the cost summary, the models and the keys, so the list waits for those answers.
            !cost.isPending && !models.isPending && !keys.isPending
              && <SetupChecklist title="Set up the AI Gateway" steps={steps} />
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
                  {list.map((p) => (
                    <tr key={p.slug}>
                      <td className="col-name"><Link to={`/gateway/providers/${p.slug}`}>{p.name}</Link></td>
                      <td>
                        {/* The same words as the Providers page. */}
                        <StatusBadge state={providerState(p)} />
                      </td>
                      <td className="mono">{fmtInt(p.requests_24h)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
      <Toaster />
    </div>
  );
}
