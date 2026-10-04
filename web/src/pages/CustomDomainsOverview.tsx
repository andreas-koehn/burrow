import { useQuery, useQueries } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { apiFetch } from "@/lib/api";
import { Badge, EmptyState, PageHeader } from "@/components/ds";
import { InfoHint } from "@/components/ds";
import { GLOSSARY } from "@/lib/glossary";
import { formatTimestamp } from "@/lib/format";
import { statusBadgeKind, STATUS_LABEL } from "@/pages/CustomDomains";
import type { Service, CustomDomain } from "@/lib/contract";

interface DomainRow {
  serviceId: string;
  serviceName: string;
  domain: CustomDomain;
}

const BACK = { to: "/settings", label: "Settings" } as const;

export default function CustomDomainsOverview() {
  const { data: services } = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    staleTime: 30_000,
  });

  const domainQueries = useQueries({
    queries: (services ?? []).map((s) => ({
      queryKey: ["service", s.id, "domains"] as const,
      queryFn: () => apiFetch<CustomDomain[]>(`/services/${s.id}/domains`),
      retry: false,
      staleTime: 30_000,
    })),
  });

  const rows: DomainRow[] = [];
  (services ?? []).forEach((s, i) => {
    const result = domainQueries[i];
    const domains = result?.data ?? [];
    for (const domain of domains) {
      rows.push({ serviceId: s.id, serviceName: s.name, domain });
    }
  });

  return (
    <div className="account-page">
      <div className="page-header-row">
        <PageHeader
          back={BACK}
          title="Custom domains (all services)"
          subtitle="Read-only roll-up — open a service to add or remove."
        />
        <InfoHint label="Custom domains" content={GLOSSARY.customDomain} />
      </div>

      {rows.length === 0 ? (
        <EmptyState title="No custom domains">
          Add a custom domain from the service detail page.
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Custom domains (all services)">
            <thead>
              <tr>
                <th>Service</th>
                <th>Hostname</th>
                <th>Status</th>
                <th>Expires</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ serviceId, serviceName, domain }) => (
                <tr key={domain.id}>
                  <td>
                    <Link to={`/services/${serviceId}/domains`}>{serviceName}</Link>
                  </td>
                  <td className="mono small">{domain.hostname}</td>
                  <td>
                    <Badge kind={statusBadgeKind(domain.status)}>
                      {STATUS_LABEL[domain.status]}
                    </Badge>
                  </td>
                  <td>{formatTimestamp(domain.not_after)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
