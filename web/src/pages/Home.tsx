import { useQuery, useQueries } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { PageHeader, MetricStrip, MetricTile, ErrorNotice } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { GLOSSARY } from "@/lib/glossary";
import type { ClientView, CostSummary, Service, Budget, CustomDomain } from "@/lib/contract";

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
}

export default function Home() {
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";

  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    enabled: isAdmin,
    retry: false,
  });

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });

  const cost = useQuery({
    queryKey: ["cost", "summary", "today"],
    queryFn: () => apiFetch<CostSummary>("/cost/summary?window=today"),
    retry: false,
    staleTime: 60_000,
  });

  const costAbsent = cost.error instanceof ApiError && cost.error.status === 404;

  const svc = Array.isArray(services.data) ? services.data : [];

  // Admin-only: SMTP + budget alerts
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: () => apiFetch<Record<string, string>>("/settings"),
    enabled: isAdmin,
    retry: false,
  });

  const budgets = useQuery({
    queryKey: ["budgets"],
    queryFn: () => apiFetch<Budget[]>("/budgets"),
    enabled: isAdmin,
    retry: false,
  });

  // Cert fan-out: only over http services
  const httpSvcs = svc.filter((s) => s.type === "http");
  const domainQueries = useQueries({
    queries: httpSvcs.map((s) => ({
      queryKey: ["service", s.id, "domains"] as const,
      queryFn: () => apiFetch<CustomDomain[]>(`/services/${s.id}/domains`),
      retry: false,
      staleTime: 30_000,
    })),
  });

  // Flatten all domain results and find expiring ones
  const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;
  const expiringDomains: { domain: CustomDomain; svcId: string }[] = [];
  domainQueries.forEach((q, i) => {
    const svcId = httpSvcs[i]!.id;
    const domains = Array.isArray(q.data) ? q.data : [];
    for (const d of domains) {
      const isExpiring =
        d.status === "cert_expiring" ||
        d.status === "cert_expired" ||
        (d.not_after && new Date(d.not_after).getTime() - Date.now() < THIRTY_DAYS_MS);
      if (isExpiring) expiringDomains.push({ domain: d, svcId });
    }
  });

  // Alert conditions
  const smtpAlert = isAdmin && settings.data !== undefined && !settings.data["smtp.host"];
  const exceededBudgets = Array.isArray(budgets.data)
    ? budgets.data.filter((b) => b.exceeded)
    : [];
  const budgetAlert = isAdmin && exceededBudgets.length > 0;
  const certAlert = expiringDomains.length > 0;
  const liveTunnels = svc.filter((s) => s.connected).length;

  const trafficValue = (() => {
    if (!isAdmin || !clients.data) return "—";
    const cls = Array.isArray(clients.data) ? clients.data : [];
    const totalIn = cls.reduce((a, c) => a + c.total_bytes_in, 0);
    const totalOut = cls.reduce((a, c) => a + c.total_bytes_out, 0);
    return `${formatBytes(totalIn)} / ${formatBytes(totalOut)}`;
  })();

  return (
    <div className="home-page">
      <PageHeader title="Overview" subtitle="Your relay at a glance." />

      <MetricStrip ariaLabel="Overview">
        <MetricTile
          label="Clients"
          value={isAdmin ? String(clients.data?.length ?? 0) : "—"}
          sub={GLOSSARY.client.split(" that ")[0]}
          tooltip="Machines running burrow connect."
        />
        <MetricTile
          label="Services"
          value={String(svc.length)}
          sub={GLOSSARY.service.split("; it")[0]}
        />
        <MetricTile
          label="Live tunnels"
          value={String(liveTunnels)}
          sub={GLOSSARY.tunnel.split(" — it")[0]}
        />
      </MetricStrip>

      <MetricStrip ariaLabel="Last 24 hours">
        <MetricTile
          label="Traffic (24h)"
          value={trafficValue}
        />
        {!costAbsent && (
          <MetricTile
            label="AI cost (24h)"
            value={cost.data ? fmtUsd(cost.data.total_usd) : "—"}
            sub={cost.data ? `${fmtInt(cost.data.tokens_in)} → ${fmtInt(cost.data.tokens_out)}` : undefined}
            tooltip="Estimates from the bundled pricing table — operator-overridable."
          />
        )}
      </MetricStrip>

      {(smtpAlert || budgetAlert || certAlert) && (
        <div className="alerts-strip">
          {smtpAlert && (
            <ErrorNotice
              variant="warn"
              role="status"
              action={<Link to="/settings">Set up email →</Link>}
            >
              Email isn't set up — user invites/password resets are unavailable.
            </ErrorNotice>
          )}
          {budgetAlert && (
            <ErrorNotice
              variant="error"
              role="status"
              action={<Link to="/cost">View budgets →</Link>}
            >
              {exceededBudgets.length} budget(s) exceeded.
            </ErrorNotice>
          )}
          {certAlert && (
            <ErrorNotice
              variant="warn"
              role="status"
              action={<Link to={`/services/${expiringDomains[0]!.svcId}/domains`}>Review →</Link>}
            >
              A custom-domain certificate is expiring.
            </ErrorNotice>
          )}
        </div>
      )}
    </div>
  );
}
