import { useQuery } from "@tanstack/react-query";
import { PageHeader, MetricStrip, MetricTile } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { apiFetch, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { GLOSSARY } from "@/lib/glossary";
import type { ClientView, CostSummary, Service } from "@/lib/contract";

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
    </div>
  );
}
