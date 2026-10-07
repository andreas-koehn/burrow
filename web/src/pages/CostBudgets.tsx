import { useEffect, useState } from "react";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, MetricStrip, MetricTile, NotAuthorized, PageHeader, Segmented, Select, SkeletonRows, TableEmptyRow } from "@/components/ds";
import UsageBreakdown from "@/components/UsageBreakdown";
import { useAuth } from "@/auth/useAuth";
import { fmtCount, fmtUsd, shortId } from "@/lib/costFormat";
import type { AiGatewayKey, AiModel, Budget, BudgetScope, CostSummary, Service } from "@/lib/contract";

type Window = CostSummary["window"];

const WINDOWS: Window[] = ["today", "week", "month", "year"];
const WINDOW_LABEL: Record<Window, string> = {
  today: "Today", week: "Week", month: "Month", year: "Year",
};

const SCOPE_OPTIONS = [
  { value: "api_key", label: "API key" },
  { value: "service", label: "Service" },
  { value: "user", label: "User" },
  { value: "global", label: "Global" },
  { value: "gateway_key", label: "Gateway key" },
  { value: "model", label: "Model" },
];
const SCOPE_LABEL: Record<BudgetScope, string> = {
  api_key: "API key", service: "Service", user: "User", global: "Global", gateway_key: "Gateway key", model: "Model",
};
const ACTION_LABEL: Record<string, string> = {
  alert_webhook: "Alert webhook", throttle_zero: "Throttle to zero", disable_key: "Disable key",
};
const ACTION_OPTIONS = [
  { value: "alert_webhook", label: "Alert webhook" },
  { value: "throttle_zero", label: "Throttle to zero" },
  { value: "disable_key", label: "Disable key" },
];

// What each action does, in words. disable_key revokes for good.
function actionHelp(action: Budget["action_on_exceed"], scope: BudgetScope): string {
  switch (action) {
    case "alert_webhook":
      return "Sends a budget.exceeded webhook event. Requests keep working.";
    case "throttle_zero":
      return scope === "gateway_key" || scope === "model"
        ? "Requests are refused until midnight UTC, when the day's budget starts over."
        : "Throttles the subject to zero requests for the rest of the day.";
    case "disable_key":
      if (scope === "api_key" || scope === "gateway_key") {
        return "Revokes the key permanently. It does not come back at midnight; you would have to create a new key.";
      }
      return scope === "model"
        ? "Requests for the model are refused until midnight UTC. A model budget only blocks; no key is revoked."
        : "Requests are blocked until midnight UTC. For this scope no key is revoked.";
  }
}

const OTHER_MODEL = "__other__";

function pctClass(pct: number | null): string {
  if (pct == null) return "ok";
  if (pct >= 0.8) return "danger";
  if (pct >= 0.5) return "warn";
  return "ok";
}

function SpendTile({ w, summary }: { w: Window; summary: CostSummary | undefined }) {
  return (
    <MetricTile
      label={WINDOW_LABEL[w]}
      value={summary ? fmtUsd(summary.total_usd) : "—"}
      sub={summary ? `${fmtCount(summary.tokens_in)} tokens in · ${fmtCount(summary.tokens_out)} out` : "—"}
    >
      <div className="pct-bar">
        <span
          className={`fill ${pctClass(summary?.pct_of_budget ?? null)}`}
          style={{ width: `${Math.min(100, Math.max(0, (summary?.pct_of_budget ?? 0) * 100))}%` }}
        />
      </div>
    </MetricTile>
  );
}

export default function CostBudgets() {
  const qc = useQueryClient();
  const { user, loading: authLoading } = useAuth();
  const isAdmin = user?.role === "admin";
  const cost = useQueries({
    queries: WINDOWS.map((w) => ({
      queryKey: ["cost", "summary", w],
      queryFn: () => apiFetch<CostSummary>(`/cost/summary?window=${w}`),
      retry: false,
      staleTime: 60_000,
    })),
  });
  const budgets = useQuery({
    queryKey: ["budgets"],
    queryFn: () => apiFetch<Budget[]>("/budgets"),
    retry: false,
    enabled: isAdmin,
  });
  // P1-10 — feature gating: if /budgets 404s, the AI gateway isn't on this
  // relay. Disable "New budget" + tooltip, keep the spend tiles since
  // /cost/summary may still respond (operators sometimes ship cost without
  // budgets).
  const featureAbsent = budgets.error instanceof ApiError && budgets.error.status === 404;

  const [usageWindow, setUsageWindow] = useState<Window>("today");
  const [addOpen, setAddOpen] = useState(false);
  const [scope, setScope] = useState<Budget["scope"]>("api_key");
  const [subjectId, setSubjectId] = useState("");
  const [dailyUsd, setDailyUsd] = useState("");
  const [dailyTokens, setDailyTokens] = useState("");
  const [modelAddr, setModelAddr] = useState("");
  const [action, setAction] = useState<Budget["action_on_exceed"]>("alert_webhook");
  const [err, setErr] = useState<string | null>(null);

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
    enabled: addOpen,
  });
  const serviceOptions = (Array.isArray(services.data) ? services.data : [])
    .map((s) => ({ value: s.id, label: s.name }));

  const keys = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
    enabled: isAdmin,
  });
  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
    enabled: addOpen && scope === "model",
  });
  const keyList = Array.isArray(keys.data) ? keys.data : [];
  const keyName = new Map(keyList.map((k) => [k.id, k.name]));
  const keyOptions = keyList.map((k) => {
    const dup = keyList.filter((o) => o.name === k.name).length > 1;
    return { value: k.id, label: `${k.name}${dup ? ` (${k.key_prefix})` : ""}${k.revoked_at ? " (revoked)" : ""}` };
  });
  const modelOptions = [
    ...(Array.isArray(models.data) ? models.data : []).map((m) => ({ value: m.name, label: m.name })),
    { value: OTHER_MODEL, label: "Other address…" },
  ];
  const subjectOf = scope === "model" && subjectId === OTHER_MODEL ? modelAddr : subjectId;

  // Client-side checks mirror validateBudget in internal/api/cost_handlers.go.
  const usdNum = dailyUsd.trim() === "" ? 0 : Number(dailyUsd);
  const tokNum = dailyTokens.trim() === "" ? 0 : Number(dailyTokens);
  const usdErr = usdNum < 0 || Number.isNaN(usdNum) ? "daily_usd must not be negative" : undefined;
  const tokErr = tokNum < 0 ? "daily_tokens must not be negative"
    : Number.isNaN(tokNum) || !Number.isInteger(tokNum) ? "daily_tokens must be a whole number" : undefined;
  const noLimit = !usdErr && !tokErr && usdNum === 0 && tokNum === 0;
  const addrErr = scope === "model" && subjectId === OTHER_MODEL && modelAddr !== ""
    && (modelAddr !== modelAddr.trim() || /^\/|\/$|^[^/]*$/.test(modelAddr))
    ? "Use the form <provider>/<model>, without spaces around it."
    : undefined;
  const subjectMissing = scope !== "global" && !subjectOf.trim();
  // The server refuses surrounding whitespace rather than trimming it; so does the form.
  const idErr = (scope === "api_key" || scope === "user") && subjectId !== subjectId.trim()
    ? "Remove the spaces before or after the ID."
    : undefined;
  const canSubmit = !usdErr && !tokErr && !noLimit && !addrErr && !idErr && !subjectMissing;

  // Reveal-then-focus: the address field appears when "Other address…" is chosen.
  useEffect(() => {
    if (scope !== "model" || subjectId !== OTHER_MODEL) return;
    // After the select has handed focus back to its trigger.
    const t = setTimeout(() => document.getElementById("budget-model-addr")?.focus(), 0);
    return () => clearTimeout(t);
  }, [scope, subjectId]);

  const create = useMutation({
    mutationFn: () =>
      apiFetch<Budget>("/budgets", {
        method: "POST",
        body: JSON.stringify({
          scope,
          subject_id: subjectOf,
          daily_usd: usdNum,
          daily_tokens: tokNum,
          action_on_exceed: action,
        }),
      }),
    onSuccess: () => {
      toast.success("Budget created.");
      qc.invalidateQueries({ queryKey: ["budgets"] });
      setAddOpen(false);
      setSubjectId("");
      setModelAddr("");
      setDailyUsd("");
      setDailyTokens("");
      setErr(null);
    },
    onError: (e: unknown) =>
      setErr(e instanceof ApiError ? e.message : "Couldn't create budget."),
  });

  function submit() {
    if (!canSubmit) return;
    create.mutate();
  }

  if (authLoading || (isAdmin && budgets.isLoading)) {
    return (
      <div className="cost-page">
        <PageHeader title="Cost & budgets" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  const forbidden = !isAdmin || (budgets.error instanceof ApiError && budgets.error.status === 403);
  // A refused /cost/summary leaves the tiles at "—"; say why.
  const costForbidden = cost.some((q) => q.error instanceof ApiError && q.error.status === 403);
  const budgetRows = budgets.data ?? [];

  return (
    <div className="cost-page">
      <PageHeader
        title="Cost & budgets"
        subtitle="Estimates from the pricing table shipped with Burrow v0.4. Operators can edit this table in Settings."
        actions={<Button variant="secondary" size="sm" onClick={() => { void apiFetch("/cost/export?format=ndjson&window=month"); }}>Export cost report</Button>}
      />

      <MetricStrip ariaLabel="Spend by window">
        {WINDOWS.map((w, i) => (
          <SpendTile key={w} w={w} summary={cost[i]!.data} />
        ))}
      </MetricStrip>
      {costForbidden && (
        <p className="muted small" role="note">
          You can&apos;t view cost data: the spend totals need the quotas:read:any permission.
        </p>
      )}

      <div className="toolbar-row">
        <Segmented aria-label="Usage period" options={WINDOWS.map((w) => ({ value: w, label: WINDOW_LABEL[w] }))} value={usageWindow} onChange={setUsageWindow} />
      </div>
      <UsageBreakdown window={usageWindow} />

      {forbidden ? (
        <section className="card">
          <h2>Budgets</h2>
          <NotAuthorized title="You can't view budgets">Budgets are visible to admins. The cost figures above are not affected.</NotAuthorized>
        </section>
      ) : budgets.isError && !featureAbsent ? (
        <section className="card">
          <h2>Budgets</h2>
          <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void budgets.refetch()}>Retry</Button>}>
            {budgets.error instanceof Error ? budgets.error.message : "Couldn't load budgets."}
          </ErrorNotice>
        </section>
      ) : (
      <section className="card">
        <h2>Budgets</h2>
        <div className="toolbar-row">
          <Button
            variant="primary"
            size="sm"
            disabled={featureAbsent}
            title={featureAbsent ? "Budget creation requires the AI gateway." : undefined}
            onClick={() => { setAddOpen(true); setErr(null); }}
          >
            New budget
          </Button>
        </div>
        <div className="table-wrap">
          <table className="data" aria-label="Budgets">
            <thead><tr><th scope="col">Scope</th><th scope="col">Subject</th><th scope="col" className="col-num">Limit</th><th scope="col">On exceed</th><th scope="col" className="col-num">Spend today</th><th scope="col">Status</th></tr></thead>
            <tbody>
              {featureAbsent
                ? <TableEmptyRow colSpan={6} title="Budgets aren't available on this relay." />
                : budgetRows.length === 0
                  ? <TableEmptyRow colSpan={6} title="No budgets yet.">Add a budget to cap daily spend or tokens per key, model, service or user.</TableEmptyRow>
                  : budgetRows.map((b) => (
                      <tr key={b.id}>
                        <td>{SCOPE_LABEL[b.scope] ?? b.scope}</td>
                        <td className="mono">{b.scope === "gateway_key" ? (keyName.get(b.subject_id) ?? `key ${shortId(b.subject_id)}`) : b.subject_id}</td>
                        <td className="mono col-num">
                          {b.daily_usd > 0 && <div>{fmtUsd(b.daily_usd)}</div>}
                          {b.daily_tokens > 0 && <div>{fmtCount(b.daily_tokens)} tokens</div>}
                        </td>
                        <td>{ACTION_LABEL[b.action_on_exceed] ?? b.action_on_exceed}</td>
                        <td className="mono col-num">
                          {b.daily_usd > 0 && <div>{fmtUsd(b.current_usd)} / {fmtUsd(b.daily_usd)}</div>}
                          {b.daily_tokens > 0 && <div>{fmtCount(b.current_tokens)} / {fmtCount(b.daily_tokens)} tokens</div>}
                        </td>
                        <td>{b.exceeded ? <Badge kind="danger">Exceeded</Badge> : <span className="muted">Within limit</span>}</td>
                      </tr>
                    ))}
            </tbody>
          </table>
        </div>
      </section>
      )}

      {isAdmin && <Dialog
        open={addOpen}
        onOpenChange={(o) => { setAddOpen(o); if (!o) setErr(null); }}
        title="New budget"
        footer={
          <>
            <Button variant="secondary" onClick={() => setAddOpen(false)}>Cancel</Button>
            <Button variant="primary" disabled={create.isPending || !canSubmit} onClick={submit}>
              Create
            </Button>
          </>
        }
      >
        <FormFieldGroup>
          <FormField label="Scope" htmlFor="budget-scope" w="md">
            <Select id="budget-scope" value={scope} onChange={(v) => { setScope(v as Budget["scope"]); setSubjectId(""); }} options={SCOPE_OPTIONS} />
          </FormField>
          {scope === "service" ? (
            <FormField
              label="Subject"
              htmlFor="budget-subject"
              w="md"
              descId="budget-subject-desc"
              help={services.isLoading ? "Loading services…" : "The service this budget applies to."}
              error={services.isError && !services.data ? "Couldn't load services." : undefined}
            >
              <Select id="budget-subject" aria-describedby="budget-subject-desc" value={subjectId} onChange={setSubjectId} options={serviceOptions} placeholder="Select a service…" />
            </FormField>
          ) : scope === "gateway_key" ? (
            <FormField
              label="Gateway key"
              htmlFor="budget-subject"
              w="md"
              descId="budget-subject-desc"
              help="Picked by name; the budget follows the key's id."
              error={keys.isError && !keys.data ? "Couldn't load gateway keys." : undefined}
            >
              <Select id="budget-subject" aria-describedby="budget-subject-desc" value={subjectId} onChange={setSubjectId} options={keyOptions} placeholder="Select a key…" />
            </FormField>
          ) : scope === "model" ? (
            <>
              <FormField
                label="Model"
                htmlFor="budget-subject"
                w="md"
                descId="budget-subject-desc"
                help="A model name from this relay, or another address."
                error={models.isError && !models.data ? "Couldn't load models." : undefined}
              >
                <Select id="budget-subject" aria-describedby="budget-subject-desc" value={subjectId} onChange={setSubjectId} options={modelOptions} placeholder="Select a model…" />
              </FormField>
              {subjectId === OTHER_MODEL && (
                <FormField
                  label="Model address"
                  htmlFor="budget-model-addr"
                  w="md"
                  descId="budget-model-addr-desc"
                  help="<provider>/<model>, for example zai/glm-5.1. Covers the gateway endpoints and /ai/<provider>/."
                  error={addrErr}
                >
                  <Input id="budget-model-addr" className="mono" value={modelAddr} aria-invalid={addrErr ? true : undefined} aria-describedby="budget-model-addr-desc" onChange={(e) => setModelAddr(e.target.value)} />
                </FormField>
              )}
            </>
          ) : scope !== "global" ? (
            <FormField
              label="Subject"
              htmlFor="budget-subject"
              w="md"
              descId="budget-subject-desc"
              help={scope === "api_key" ? "ID of the API key." : "ID of the user."}
              error={idErr}
            >
              <Input id="budget-subject" className="mono" aria-describedby="budget-subject-desc" aria-invalid={idErr ? true : undefined} value={subjectId} onChange={(e) => setSubjectId(e.target.value)} />
            </FormField>
          ) : null}
          <FormField label="Daily USD" htmlFor="budget-daily" w="sm" error={usdErr} descId="budget-usd-desc">
            <Input id="budget-daily" type="number" min="0" step="any" className="mono" value={dailyUsd} aria-invalid={usdErr ? true : undefined} aria-describedby={usdErr ? "budget-limit-hint budget-usd-desc" : "budget-limit-hint"} onChange={(e) => setDailyUsd(e.target.value)} />
          </FormField>
          <FormField label="Daily tokens" htmlFor="budget-tokens" w="sm" error={tokErr} descId="budget-tokens-desc">
            <Input id="budget-tokens" type="number" min="0" step="1" className="mono" value={dailyTokens} aria-invalid={tokErr ? true : undefined} aria-describedby={tokErr ? "budget-limit-hint budget-tokens-desc" : "budget-limit-hint"} onChange={(e) => setDailyTokens(e.target.value)} />
          </FormField>
          <p id="budget-limit-hint" className="muted small">
            {noLimit ? "Set a daily amount in USD, in tokens, or both." : "Either limit alone is enough. Budgets reset at midnight UTC."}
          </p>
          <FormField label="Action on exceed" htmlFor="budget-action" w="md">
            <Select id="budget-action" aria-describedby="budget-action-help" value={action} onChange={(v) => setAction(v as Budget["action_on_exceed"])} options={ACTION_OPTIONS} />
          </FormField>
          <p id="budget-action-help" className="muted small">{actionHelp(action, scope)}</p>
        </FormFieldGroup>
        {err && <p role="alert" className="notice-inline error">{err}</p>}
      </Dialog>}
      <Toaster />
    </div>
  );
}
