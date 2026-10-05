import { useState } from "react";
import { Link, useParams, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, ErrorNotice, PageHeader, SkeletonRows, Tabs } from "@/components/ds";
import { ServiceUrl } from "@/components/ServiceUrl";
import { EditSlugDialog } from "@/components/EditSlugDialog";
import { AccessModePanel } from "@/components/AccessModePanel";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";
import { UpstreamCredentialsPanel } from "@/pages/UpstreamCredentials";
import type { ServiceDetail as ServiceDetailType, AccessMode } from "@/lib/contract";

const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
};

const BACK = { to: "/services", label: "Services" } as const;

export default function ServiceDetail() {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const initialTab = location.hash === "#upstream-key"
    ? "upstream-key"
    : "access";
  const [tab, setTab] = useState(initialTab);
  const [editSlug, setEditSlug] = useState(false);

  const { data: svc, isLoading, error, refetch } = useQuery({
    queryKey: ["service", id],
    queryFn: () => apiFetch<ServiceDetailType>(`/services/${id}`),
    enabled: !!id,
    retry: false,
  });

  if (isLoading) {
    return (
      <div className="service-detail-page">
        <PageHeader back={BACK} title="Service" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  if (error || !svc) {
    return (
      <div className="service-detail-page">
        <PageHeader back={BACK} title="Service" />
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

  // A pre-provisioned service has no name until a title is set or a client connects.
  const label = svc.name || svc.id;

  return (
    <div className="service-detail-page">
      <PageHeader back={BACK} title={`Service · ${label}`} />

      {/* Meta strip */}
      <div className="meta-strip">
        {svc.type === "http" && (svc.slug || svc.url) && <ServiceUrl slug={svc.slug} url={svc.url} />}
        {svc.type === "http" && (
          <Button variant="secondary" size="sm" onClick={() => setEditSlug(true)}>Edit URL</Button>
        )}
        <Badge kind={`access-${svc.access_mode}`} nodot>
          {ACCESS_LABEL[svc.access_mode]}
        </Badge>
        {svc.connected
          ? <Badge kind="status-connected">connected</Badge>
          : <Badge kind="status-idle">idle</Badge>}
      </div>

      {svc.type === "http" && (svc.slug || svc.url) && (
        <p className="muted small">
          Reached at the URL above, on the same origin as this dashboard. Only expose apps you trust:
          a page served here can act as the signed-in dashboard user.
        </p>
      )}

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
                serviceName={label}
                mode={svc.access_mode}
                clientId={`svc:${svc.id}`}
                hideApiKeys
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
        ]}
      />
      <EditSlugDialog service={{ id: svc.id, name: label, slug: svc.slug }} open={editSlug} onOpenChange={setEditSlug} />
    </div>
  );
}
