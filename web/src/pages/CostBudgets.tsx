import { useState } from "react";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, FormField, FormFieldGroup, Input, MetricStrip, MetricTile, PageHeader, Select, SkeletonRows, TableEmptyRow } from "@/components/ds";
import type { Budget, CostSummary, Service } from "@/lib/contract";

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
];
const ACTION_OPTIONS = [
  { value: "alert_webhook", label: "Alert webhook" },
  { value: "throttle_zero", label: "Throttle to zero" },
  { value: "disable_key", label: "Disable key" },
];

function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
}

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
      sub={summary ? `${summary.tokens_in.toLocaleString()} tokens in · ${summary.tokens_out.toLocaleString()} out` : "—"}
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
  });
  // P1-10 — feature gating: if /budgets 404s, the AI gateway isn't on this
  // relay. Disable "New budget" + tooltip, keep the spend tiles since
  // /cost/summary may still respond (operators sometimes ship cost without
  // budgets).
  const featureAbsent = budgets.error instanceof ApiError && budgets.error.status === 404;

  const [addOpen, setAddOpen] = useState(false);
  const [scope, setScope] = useState<Budget["scope"]>("api_key");
  const [subjectId, setSubjectId] = useState("");
  const [dailyUsd, setDailyUsd] = useState("");
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

  const create = useMutation({
    mutationFn: () =>
      apiFetch<Budget>("/budgets", {
        method: "POST",
        body: JSON.stringify({
          scope,
          subject_id: subjectId,
          daily_usd: Number(dailyUsd),
          action_on_exceed: action,
        }),
      }),
    onSuccess: () => {
      toast.success("Budget created.");
      qc.invalidateQueries({ queryKey: ["budgets"] });
      setAddOpen(false);
      setSubjectId("");
      setDailyUsd("");
      setErr(null);
    },
    onError: (e: unknown) =>
      setErr(e instanceof ApiError ? e.message : "Couldn't create budget."),
  });

  function submit() {
    if (Number(dailyUsd) <= 0) {
      setErr("daily_usd must be greater than zero");
      return;
    }
    create.mutate();
  }

  if (!budgets.data && !featureAbsent) {
    return (
      <div className="cost-page">
        <PageHeader title="Cost & budgets" />
        <SkeletonRows n={4} />
      </div>
    );
  }

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
            <thead><tr><th>Scope</th><th>Subject</th><th className="col-num">Daily $</th><th>On exceed</th><th className="col-num">Spend</th></tr></thead>
            <tbody>
              {featureAbsent
                ? <TableEmptyRow colSpan={5} title="Budgets aren't available on this relay." />
                : (budgets.data ?? []).length === 0
                  ? <TableEmptyRow colSpan={5} title="No budgets yet.">Add a budget to cap daily spend per key, service or user.</TableEmptyRow>
                  : (budgets.data ?? []).map((b) => (
                      <tr key={b.id}>
                        <td>{b.scope}</td>
                        <td className="mono">{b.subject_id}</td>
                        <td className="mono col-num">{fmtUsd(b.daily_usd)}</td>
                        <td>{b.action_on_exceed}</td>
                        <td className="mono col-num">{fmtUsd(b.current_usd)}</td>
                      </tr>
                    ))}
            </tbody>
          </table>
        </div>
      </section>

      <Dialog
        open={addOpen}
        onOpenChange={(o) => { setAddOpen(o); if (!o) setErr(null); }}
        title="New budget"
        footer={
          <>
            <Button variant="secondary" onClick={() => setAddOpen(false)}>Cancel</Button>
            <Button variant="primary" disabled={create.isPending || (scope !== "global" && !subjectId.trim())} onClick={submit}>
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
              help={services.isLoading ? "Loading services…" : "The service this budget applies to."}
              error={services.isError && !services.data ? "Couldn't load services." : undefined}
            >
              <Select id="budget-subject" value={subjectId} onChange={setSubjectId} options={serviceOptions} placeholder="Select a service…" />
            </FormField>
          ) : scope !== "global" ? (
            <FormField
              label="Subject"
              htmlFor="budget-subject"
              w="md"
              help={scope === "api_key" ? "ID of the API key." : "ID of the user."}
            >
              <Input id="budget-subject" className="mono" value={subjectId} onChange={(e) => setSubjectId(e.target.value)} />
            </FormField>
          ) : null}
          <FormField label="Daily USD" htmlFor="budget-daily" w="sm">
            <Input id="budget-daily" type="number" className="mono" value={dailyUsd} onChange={(e) => setDailyUsd(e.target.value)} />
          </FormField>
          <FormField label="Action on exceed" htmlFor="budget-action" w="md">
            <Select id="budget-action" value={action} onChange={(v) => setAction(v as Budget["action_on_exceed"])} options={ACTION_OPTIONS} />
          </FormField>
        </FormFieldGroup>
        {err && <p role="alert" className="notice-inline error">{err}</p>}
      </Dialog>
      <Toaster />
    </div>
  );
}
