import { useQuery } from "@tanstack/react-query";
import { PageHeader, MetricStrip, MetricTile } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";
import { apiFetch } from "@/lib/api";
import { GLOSSARY } from "@/lib/glossary";
import type { ClientView, Service } from "@/lib/contract";

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

  const svc = Array.isArray(services.data) ? services.data : [];
  const liveTunnels = svc.filter((s) => s.connected).length;

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
    </div>
  );
}
