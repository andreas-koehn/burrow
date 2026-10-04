import { useEffect, useImperativeHandle, useState, type Ref } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { AccessModeCard, Button, ErrorNotice, FormField, FormFieldGroup, Input } from "@/components/ds";
import { type AccessMode } from "@/lib/contract";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";
import { AccessPolicyEditor } from "@/components/AccessPolicyEditor";
import { IPGeoPanel } from "@/components/IPGeoPanel";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

const SELECTABLE = ["open", "api_key", "burrow_login"] as const;

// User-facing mode explainers. P1-3: dropped the "v0.2.0" leak — the help
// text no longer references internal version numbers. Visitors are described
// in terms of what Burrow does to their requests, not the codebase history.
const META: Record<AccessMode, { title: string; help: string }> = {
  open: { title: "Open — raw passthrough", help: "Burrow adds no auth — visitor traffic flows straight through. The safe default for TCP tunnels." },
  api_key: { title: "API key — header check", help: "Burrow verifies an API key header before proxying." },
  burrow_login: { title: "Burrow login — role-based", help: "Visitors sign in with their Burrow account at a hosted gate." },
  mtls: { title: "mTLS — client certificate", help: "Burrow terminates TLS and requires a client certificate chained to a CA you upload." },
};

// AccessModePanelHandle is the imperative handle exposed via panelRef so the
// surrounding Dialog footer (Tunnels / Services Configure flows) can drive
// Save without re-implementing the mutation. Pages that embed the panel
// inline (ServiceDetail) ignore this — the inline Save button stays visible
// when no panelRef is supplied (P1-7).
export interface AccessModePanelHandle {
  save: () => void;
  isSaving: boolean;
}

export interface AccessModePanelProps {
  serviceId: string;
  serviceName: string;
  mode: AccessMode;
  clientId?: string;
  // Imperative handle for the Configure dialogs. When provided, the inline
  // "Save changes" button is hidden — the dialog footer is expected to
  // trigger save.
  panelRef?: Ref<AccessModePanelHandle>;
  // Hosts that already show API keys elsewhere (service detail has an
  // "API keys" tab) pass this so the list is not rendered twice.
  hideApiKeys?: boolean;
}

export function AccessModePanel({ serviceId, serviceName, mode, clientId, panelRef, hideApiKeys }: AccessModePanelProps) {
  const qc = useQueryClient();
  const [selected, setSelected] = useState<AccessMode | null>(mode === "mtls" ? null : mode);
  // RFC 7230 header-name tokens disallow colon + whitespace; the prior
  // default "Authorization: Bearer" persisted as an uninterpretable header
  // and the backend (api/service_handlers.go: isValidHTTPHeaderName) now
  // rejects it with 400. "Authorization" is the proxy's safe default; it
  // also strips a "Bearer " prefix for that header only.
  const [apiKeyHeader, setApiKeyHeader] = useState("Authorization");
  const [err, setErr] = useState<string | null>(null);

  function buildBody(): Record<string, unknown> {
    if (selected === "api_key") return { access_mode: selected, api_key_header: apiKeyHeader };
    return { access_mode: selected };
  }

  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/services/${serviceId}/access-mode`, {
        method: "PUT",
        body: JSON.stringify(buildBody()),
      }),
    onSuccess: () => {
      setErr(null);
      qc.invalidateQueries({ queryKey: ["services"] });
      qc.invalidateQueries({ queryKey: ["service", serviceId] });
      qc.invalidateQueries({ queryKey: ["tunnels"] });
      if (clientId) qc.invalidateQueries({ queryKey: ["client", clientId] });
      toast.success(`Access settings saved. ${serviceName} · mode: ${selected}`);
    },
    onError: (e: unknown) => {
      if (e instanceof ApiError && e.status === 403) {
        setErr("You don't have permission to configure this service.");
      } else if (e instanceof ApiError) {
        setErr(e.message);
      } else {
        setErr("Couldn't save access settings.");
      }
    },
  });

  // Expose save/isSaving on the imperative handle so Dialog footers can drive
  // them without lifting the entire mutation. Refreshed whenever isPending
  // changes so the footer button's disabled state stays in sync (P1-7).
  useImperativeHandle(
    panelRef,
    () => ({ save: () => { if (selected) save.mutate(); }, isSaving: save.isPending }),
    [save, selected],
  );

  // Re-publish on save state transitions so the dialog footer can read the
  // latest isSaving flag through its own ref-mirror.
  useEffect(() => { /* dependency carrier for save.isPending updates */ }, [save.isPending]);

  return (
    <div className="access-panel">
      <FormFieldGroup>
        {mode === "mtls" && (
          <ErrorNotice variant="info" role="note">
            This service is set to mTLS, which is inactive while services are reached by path. Choose another mode.
          </ErrorNotice>
        )}
        <div role="radiogroup" aria-label="Access mode" className="mode-list">
          {SELECTABLE.map((m) => {
            const meta = META[m];
            return (
              <AccessModeCard
                key={m}
                title={meta.title}
                description={meta.help}
                selected={selected === m}
                onSelect={() => setSelected(m)}
              />
            );
          })}
        </div>

        {selected === "api_key" && (
          <div className="mode-detail">
            <FormField label="API key header" htmlFor={`api-key-header-${serviceId}`} w="md">
              <Input
                id={`api-key-header-${serviceId}`}
                className="mono"
                value={apiKeyHeader}
                onChange={(e) => setApiKeyHeader(e.target.value)}
              />
            </FormField>
            {!hideApiKeys && <ApiKeysPanel serviceId={serviceId} />}
          </div>
        )}

        {selected === "burrow_login" && (
          <div className="mode-detail">
            <AccessPolicyEditor serviceId={serviceId} />
          </div>
        )}

        {err && <p role="alert" className="notice-inline">{err}</p>}

        <IPGeoPanel serviceId={serviceId} />

        {/* P1-7: hide the inline button when the panel is hosted inside a
            Dialog. The dialog footer carries Cancel + Save changes instead.
            The ServiceDetail access tab embeds the panel inline and
            still relies on the inline button. */}
        {!panelRef && (
          <div className="panel-actions">
            <Button variant="primary" size="sm" disabled={save.isPending || !selected} onClick={() => save.mutate()}>
              {save.isPending ? "Saving…" : "Save changes"}
            </Button>
          </div>
        )}
      </FormFieldGroup>
      <Toaster />
    </div>
  );
}
