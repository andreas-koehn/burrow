import { useMemo, useState } from "react";
import { useQuery, useQueries } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { PageHeader, MetricStrip, MetricTile, ErrorNotice, Button, SkeletonRows } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { EMAIL_NOT_CONFIGURED } from "@/lib/copy";
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

  // Fix C: Date.now() is an impure call. Capture it once in a lazy-initialized
  // state value so it is never called directly during the render pass.
  // (The lazy initializer runs only once at mount, outside the render cycle.)
  const [nowMs] = useState<number>(() => Date.now());
  const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;
  const expiringDomains = useMemo(() => {
    const result: { domain: CustomDomain; svcId: string }[] = [];
    domainQueries.forEach((q, i) => {
      const svcId = httpSvcs[i]!.id;
      const domains = Array.isArray(q.data) ? q.data : [];
      for (const d of domains) {
        const isExpiring =
          d.status === "cert_expiring" ||
          d.status === "cert_expired" ||
          (d.not_after && new Date(d.not_after).getTime() - nowMs < THIRTY_DAYS_MS);
        if (isExpiring) result.push({ domain: d, svcId });
      }
    });
    return result;
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [domainQueries, httpSvcs.length, nowMs]);

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

  const quickActions = (
    <div className="home-quick-actions">
      <Link to="/clients/connect">
        <Button variant="primary" size="sm">Connect a client</Button>
      </Link>
      <Link to="/services">
        <Button variant="secondary" size="sm">New service</Button>
      </Link>
    </div>
  );

  // Loading guards: each strip independently shows a skeleton while its data resolves.
  const overviewLoading = services.isLoading || (isAdmin && clients.isLoading);
  const trafficLoading = isAdmin && clients.isLoading;

  return (
    <div className="home-page">
      <PageHeader title="Overview" subtitle="Your relay at a glance." actions={quickActions} />

      {overviewLoading ? (
        <SkeletonRows n={3} />
      ) : (
        <MetricStrip ariaLabel="Overview">
          <MetricTile
            label="Clients"
            value={isAdmin ? String(clients.data?.length ?? 0) : "—"}
            sub="machines connected"
            tooltip={GLOSSARY.client.replace(/`/g, "")}
          />
          <MetricTile
            label="Services"
            value={String(svc.length)}
            sub="saved configurations"
            tooltip={GLOSSARY.service}
          />
          <MetricTile
            label="Live tunnels"
            value={String(liveTunnels)}
            sub="forwarding right now"
            tooltip={GLOSSARY.tunnel}
          />
        </MetricStrip>
      )}

      {cost.isLoading ? (
        <SkeletonRows n={2} />
      ) : (
        <MetricStrip ariaLabel="Last 24 hours">
          <MetricTile
            label="Traffic (24h)"
            value={trafficLoading ? "…" : trafficValue}
            sub="in / out"
          />
          {!costAbsent && (
            <MetricTile
              label="AI cost (24h)"
              value={cost.data ? fmtUsd(cost.data.total_usd) : "—"}
              sub={cost.data ? `${fmtInt(cost.data.tokens_in)} tokens in · ${fmtInt(cost.data.tokens_out)} out` : undefined}
              tooltip="Estimates from the bundled pricing table — operator-overridable."
            />
          )}
        </MetricStrip>
      )}

      {(smtpAlert || budgetAlert || certAlert) && (
        <div className="alerts-strip">
          {smtpAlert && (
            <ErrorNotice
              variant="warn"
              role="status"
              action={<Link to="/settings">Set up email →</Link>}
            >
              {EMAIL_NOT_CONFIGURED}
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

      <section className="home-explainer">
        <h2>How Burrow works</h2>
        <ol>
          <li>
            Run <code>burrow connect</code> on your machine — this registers it as a{" "}
            <strong>Client</strong> on the relay. Clients are machines running{" "}
            <code>burrow connect</code> that expose local services through this relay.
          </li>
          <li>
            Each Client exposes one or more local ports through durable{" "}
            <strong>Services</strong>. A Service is the saved configuration
            (access mode + hostname) that persists even when no client is connected.
          </li>
          <li>
            A live connection to a Service is a <strong>Tunnel</strong>. Tunnels exist
            only while the client is connected and actively forwarding traffic.
          </li>
          <li>
            Add an access mode (API key, mTLS) or point a Service at an
            OpenAI-compatible upstream to unlock the AI gateway.
          </li>
        </ol>
      </section>
    </div>
  );
}
