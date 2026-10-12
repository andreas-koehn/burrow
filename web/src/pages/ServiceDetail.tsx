import { useState } from "react";
import { Link, useParams, useLocation } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, ErrorNotice, PageHeader, SkeletonRows, Switch, Tabs } from "@/components/ds";
import { ServiceUrl } from "@/components/ServiceUrl";
import { EditSlugDialog } from "@/components/EditSlugDialog";
import { AccessModePanel } from "@/components/AccessModePanel";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";
import { UpstreamCredentialsPanel } from "@/pages/UpstreamCredentials";
import { ServiceClientPanel } from "@/components/ServiceClientPanel";
import { serviceState } from "@/lib/status";
import type { ServiceDetail as ServiceDetailType, AccessMode, AiProvider } from "@/lib/contract";

const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
};

const BACK = { to: "/services", label: "Services" } as const;
const TAB_VALUES = ["access", "api-keys", "upstream-key"];
// How often a service nobody serves is asked for again: the reader has the command
// in front of them and waits for the page to say Online (as WaitForProvider does).
const WAIT_MS = 2000;

export default function ServiceDetail() {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  // A fragment that names a tab opens it: the provider page links to #api-keys.
  const fragment = location.hash.slice(1);
  const initialTab = TAB_VALUES.includes(fragment) ? fragment : "access";
  const [tab, setTab] = useState(initialTab);
  const [editSlug, setEditSlug] = useState(false);
  const qc = useQueryClient();

  const { data: svc, isLoading, error, refetch } = useQuery({
    queryKey: ["service", id],
    queryFn: () => apiFetch<ServiceDetailType>(`/services/${id}`),
    enabled: !!id,
    retry: false,
    // Asked again until a client serves it, then no more. An answer that fails ends
    // the asking too: the panel says so, and a reload starts it again.
    refetchInterval: (query) => {
      if (query.state.status === "error") return false;
      const s = query.state.data;
      // The hidden service of a hosted provider never comes online: no client serves it.
      return s && s.type !== "direct" && serviceState(s) !== "online" ? WAIT_MS : false;
    },
  });

  // The switch is offered for a service that backs a model provider. The list
  // is the one the Providers pages share; a failure only hides the switch.
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    enabled: !!id,
    retry: false,
  });

  const [gatewayErr, setGatewayErr] = useState<string | null>(null);
  const setGatewayOnly = useMutation({
    mutationFn: (on: boolean) =>
      apiFetch<{ gateway_only: boolean }>(`/services/${id}/gateway-only`, {
        method: "PUT",
        body: JSON.stringify({ gateway_only: on }),
      }),
    onSuccess: async () => {
      setGatewayErr(null);
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["service", id] }),
        qc.invalidateQueries({ queryKey: ["services"] }),
        qc.invalidateQueries({ queryKey: ["tunnels"] }),
      ]);
    },
    onError: (e: unknown) => setGatewayErr(e instanceof ApiError ? e.message : "Couldn't change the setting."),
  });

  if (isLoading) {
    return (
      <div className="service-detail-page">
        <PageHeader back={BACK} title="Service" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  // Only without a service to show: one that is on the page stays there when a later answer fails.
  if (!svc) {
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
  // A gateway-only service whose provider was deleted keeps the switch, so it
  // can be turned off again.
  const backsProvider = (providers.data ?? []).some((p) => p.service_id === svc.id);
  const showGatewaySwitch = svc.type === "http" && (backsProvider || svc.gateway_only);
  // The provider is named as the gateway names it; its slug stands in where the list is not readable.
  const providerSlug = svc.provider_slug ?? "";
  const providerName = (providers.data ?? []).find((p) => p.slug === providerSlug)?.name || providerSlug;
  const hasPublicUrl = svc.type === "http" && !svc.gateway_only && !!(svc.slug || svc.url);
  // Under /svc/ a service is on the dashboard's own origin. Behind an API key no
  // browser gets a page from it; in the other two modes one does.
  const sharesOrigin = hasPublicUrl && (svc.access_mode === "open" || svc.access_mode === "burrow_login");

  return (
    <div className="service-detail-page">
      <PageHeader
        back={BACK}
        title={label}
        actions={<Link className="btn btn-secondary btn-sm" to={`/traffic?service=${encodeURIComponent(svc.id)}`}>View traffic</Link>}
      />

      {/* Meta strip */}
      <div className="meta-strip">
        {svc.type === "http" && svc.gateway_only && (
          <span className="muted">No public URL: this service answers through the AI gateway only.</span>
        )}
        {hasPublicUrl && <ServiceUrl slug={svc.slug} url={svc.url} />}
        {svc.type === "http" && !svc.gateway_only && (
          <Button variant="secondary" size="sm" onClick={() => setEditSlug(true)}>Edit URL</Button>
        )}
        <Badge kind={`access-${svc.access_mode}`} nodot>
          {ACCESS_LABEL[svc.access_mode]}
        </Badge>
        {providerSlug && (
          <Link className="link-inline" to={`/gateway/providers/${encodeURIComponent(providerSlug)}`}>
            Provider: {providerName}
          </Link>
        )}
      </div>

      {/* Only what a client serves: the hidden service of a hosted provider has no machine behind it. */}
      {svc.type !== "direct" && <ServiceClientPanel service={svc} unread={!!error} />}

      <Tabs
        value={tab}
        onChange={setTab}
        tabs={[
          {
            value: "access",
            label: "Access",
            content: (
              <div className="col gap-4">
                {showGatewaySwitch && (
                  <div className="col gap-2">
                    <label className="row row-center gap-2">
                      <Switch
                        aria-label="Reachable through the AI gateway only"
                        aria-describedby="gateway-only-help"
                        checked={svc.gateway_only}
                        disabled={setGatewayOnly.isPending}
                        onChange={(on) => setGatewayOnly.mutate(on)}
                      />
                      <span>Reachable through the AI gateway only</span>
                    </label>
                    <p id="gateway-only-help" className="muted small">
                      Closes the direct address and custom domains: they answer as if the
                      service did not exist. Models stay available under <span className="mono">/ai/</span>, <span className="mono">/openai/v1</span> and{" "}
                      <span className="mono">/anthropic</span>.
                    </p>
                    {gatewayErr && <ErrorNotice role="alert">{gatewayErr}</ErrorNotice>}
                  </div>
                )}
                {sharesOrigin && (
                  <ErrorNotice variant="warn" role="note">
                    This service shares the dashboard&apos;s address. A page it serves can act as the
                    signed-in dashboard user. Only publish apps you trust.
                  </ErrorNotice>
                )}
                <AccessModePanel
                  serviceId={svc.id}
                  serviceName={label}
                  mode={svc.access_mode}
                  clientId={`svc:${svc.id}`}
                  hideApiKeys
                />
              </div>
            ),
          },
          {
            value: "api-keys",
            label: "API keys",
            content: (
              <ApiKeysPanel
                serviceId={svc.id}
                intro={
                  <p className="muted small page-intro">
                    Apps that call this service send one of these keys.
                    {backsProvider && (
                      <>{" "}For models, use a <Link className="link-inline" to="/gateway/keys">gateway key</Link> instead.</>
                    )}
                  </p>
                }
              />
            ),
          },
          {
            value: "upstream-key",
            label: "Upstream key",
            content: (
              <>
                {/* Said only while it is not one yet; the link opens the steps that make it one. */}
                {!providerSlug && (
                  <p className="muted small" style={{ marginBottom: "var(--space-3, 12px)" }}>
                    This service can be used as a model provider — an admin can register it under{" "}
                    <Link to="/gateway/providers?new=local">Providers</Link>.
                  </p>
                )}
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
