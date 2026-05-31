import { useState } from "react";
import { Link, useParams, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, ErrorNotice, PageHeader, SkeletonRows, Tabs } from "@/components/ds";
import { AccessModePanel } from "@/components/AccessModePanel";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";
import { UpstreamCredentialsPanel } from "@/pages/UpstreamCredentials";
import { CustomDomainsPanel } from "@/pages/CustomDomains";
import type { ServiceDetail as ServiceDetailType, AccessMode } from "@/lib/contract";

const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
};

export default function ServiceDetail() {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const initialTab = location.hash === "#upstream-key"
    ? "upstream-key"
    : location.pathname.endsWith("/domains")
      ? "domains"
      : "access";
  const [tab, setTab] = useState(initialTab);

  const { data: svc, isLoading, error, refetch } = useQuery({
    queryKey: ["service", id],
    queryFn: () => apiFetch<ServiceDetailType>(`/services/${id}`),
    enabled: !!id,
    retry: false,
  });

  if (isLoading) {
    return (
      <div className="service-detail-page">
        <PageHeader title="Service" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  if (error || !svc) {
    return (
      <div className="service-detail-page">
        <PageHeader title="Service" />
        <ErrorNotice
          action={
            <button type="button" onClick={() => void refetch()}>
              Retry
            </button>
          }
        >
          {error instanceof ApiError ? error.message : "Couldn't load service."}
        </ErrorNotice>
      </div>
    );
  }

  return (
    <div className="service-detail-page">
      <PageHeader title={`Service · ${svc.name}`} />

      {/* Meta strip */}
      <div className="meta-strip">
        {svc.hostname && (
          <span className="mono small">{svc.hostname}</span>
        )}
        <Badge kind={`access-${svc.access_mode}`} nodot>
          {ACCESS_LABEL[svc.access_mode]}
        </Badge>
        {svc.connected
          ? <Badge kind="status-connected">connected</Badge>
          : <span className="muted">idle</span>}
      </div>

      <Tabs
        value={tab}
        onChange={setTab}
        tabs={[
          {
            value: "access",
            label: "Access",
            content: (
              <AccessModePanel
                serviceId={svc.id}
                serviceName={svc.name}
                mode={svc.access_mode}
                clientId={`svc:${svc.id}`}
              />
            ),
          },
          {
            value: "api-keys",
            label: "API keys",
            content: <ApiKeysPanel serviceId={svc.id} />,
          },
          {
            value: "upstream-key",
            label: "Upstream key",
            content: (
              <>
                <p className="muted small" style={{ marginBottom: "var(--space-3, 12px)" }}>
                  This makes the service an AI endpoint — see it under{" "}
                  <Link to="/ai/endpoints">AI endpoints</Link>.
                </p>
                <UpstreamCredentialsPanel
                  serviceId={svc.id}
                  serviceName={svc.name}
                />
              </>
            ),
          },
          {
            value: "domains",
            label: "Custom domains",
            content: <CustomDomainsPanel serviceId={svc.id} />,
          },
        ]}
      />
    </div>
  );
}
