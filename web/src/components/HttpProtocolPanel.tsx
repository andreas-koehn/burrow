import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { Button, ErrorNotice, FormField, Input, Select, Switch } from "@/components/ds";
import type { AccessMode, HttpSettings, HttpSettingsInput, IngressStatus, UpstreamProtocol } from "@/lib/contract";

const PROTOCOL_OPTIONS: { value: UpstreamProtocol; label: string }[] = [
  { value: "http1", label: "HTTP/1.1 (default)" },
  { value: "https", label: "HTTPS (HTTP/1.1 over TLS)" },
  { value: "h2c", label: "h2c (HTTP/2 without TLS, for gRPC)" },
  { value: "h2", label: "h2 (HTTP/2 over TLS, for gRPC)" },
];
const TLS_PROTOCOLS: readonly UpstreamProtocol[] = ["https", "h2"];

/**
 * How the relay talks to the service behind the tunnel: HTTP/1.1 unless a gRPC (HTTP/2) or TLS upstream
 * is chosen here, or named by the client. Only a holder of services:configure may read or change it.
 */
export function HttpProtocolPanel({ serviceId, accessMode }: { serviceId: string; accessMode: AccessMode }) {
  const titleId = useId();
  const canEdit = useCan([P.servicesConfigureOwn, P.servicesConfigureAny]);
  const key = ["service", serviceId, "http-settings"];
  const settings = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<HttpSettings>(`/services/${serviceId}/http-settings`),
    enabled: canEdit,
    retry: false,
  });
  // The relay's own switches; the card works without them, so a failure only hides the note.
  const ingress = useQuery({
    queryKey: ["ingress", "status"],
    queryFn: () => apiFetch<IngressStatus>("/ingress/status"),
    enabled: canEdit,
    retry: false,
    staleTime: 60_000,
  });

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Protocol</h2>
      {!canEdit ? (
        <p className="muted small">Changing how the relay speaks to this service needs the services:configure permission.</p>
      ) : settings.error ? (
        <ErrorNotice role="alert">
          {settings.error instanceof ApiError ? settings.error.message : "Couldn't load the protocol settings."}
        </ErrorNotice>
      ) : !settings.data ? null : (
        // Re-keyed by what the relay stored, so a saved form starts from the saved values.
        <ProtocolForm
          key={JSON.stringify(settings.data)}
          serviceId={serviceId}
          data={settings.data}
          accessMode={accessMode}
          h2cOff={ingress.data?.h2c === false}
          queryKey={key}
        />
      )}
    </section>
  );
}

function ProtocolForm({ serviceId, data, accessMode, h2cOff, queryKey }: {
  serviceId: string; data: HttpSettings; accessMode: AccessMode; h2cOff: boolean; queryKey: string[];
}) {
  const qc = useQueryClient();
  const id = useId();
  const [protocol, setProtocol] = useState<UpstreamProtocol>(data.upstream_protocol);
  const [verify, setVerify] = useState(data.upstream_tls_verify);
  const [serverName, setServerName] = useState(data.upstream_tls_server_name);
  const [maxDuration, setMaxDuration] = useState(String(data.grpc_max_duration_s));
  // A CA is replaced by pasting one, or cleared; left alone it is not sent.
  const [caPem, setCaPem] = useState("");
  const [removeCa, setRemoveCa] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const tls = TLS_PROTOCOLS.includes(protocol);
  const storedCa = data.upstream_tls_ca_sha256 !== "" && !removeCa;

  const save = useMutation({
    mutationFn: () => {
      const body: HttpSettingsInput = {
        upstream_protocol: protocol,
        upstream_tls_verify: verify,
        grpc_max_duration_s: Number(maxDuration) || 0,
      };
      if (serverName.trim() !== data.upstream_tls_server_name) body.upstream_tls_server_name = serverName.trim();
      if (caPem.trim() !== "") body.upstream_tls_ca_pem = caPem.trim();
      else if (removeCa) body.upstream_tls_ca_pem = "";
      return apiFetch<HttpSettings>(`/services/${serviceId}/http-settings`, { method: "PUT", body: JSON.stringify(body) });
    },
    onSuccess: async () => {
      setErr(null);
      await qc.invalidateQueries({ queryKey });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the protocol settings."),
  });

  return (
    <>
      <p className="muted small">
        How the relay speaks to the service behind the tunnel. Visitors may use HTTP/1.1, HTTP/2 or gRPC whatever you choose here.
      </p>
      <FormField label="Upstream protocol" htmlFor={`${id}-proto`} w="md">
        <Select
          id={`${id}-proto`}
          aria-label="Upstream protocol"
          options={PROTOCOL_OPTIONS}
          value={protocol}
          onChange={(v) => { setProtocol(v as UpstreamProtocol); setErr(null); }}
        />
      </FormField>

      {tls && (
        <div className="col gap-3">
          <div className="col gap-1">
            <label className="row row-center gap-2">
              <Switch
                aria-label="Verify the upstream certificate"
                aria-describedby={`${id}-verify`}
                checked={verify}
                onChange={setVerify}
              />
              <span>Verify the upstream certificate</span>
            </label>
            <p id={`${id}-verify`} className="muted small">
              {verify
                ? "The relay checks the service's certificate against the system roots and the CA below."
                : "Verification is off: the connection is encrypted but the service is not authenticated, so anyone who can reach its address can pose as it."}
            </p>
          </div>
          <FormField
            label="Server name"
            htmlFor={`${id}-sni`}
            w="md"
            help="The name the certificate is checked for; empty uses the target's host. A DNS name, not an IP address."
          >
            <Input id={`${id}-sni`} mono value={serverName} autoComplete="off" spellCheck={false}
              onChange={(e) => setServerName(e.target.value)} />
          </FormField>
          <div className="col gap-1">
            {storedCa && (
              <div className="row row-center gap-2">
                <span className="small">CA certificate stored, SHA-256 <code className="mono">{data.upstream_tls_ca_sha256}</code></span>
                <Button type="button" variant="secondary" size="sm" onClick={() => { setRemoveCa(true); setCaPem(""); }}>Remove CA</Button>
              </div>
            )}
            {removeCa && <p className="muted small">The CA is removed when you save.</p>}
            <FormField
              label="CA certificate (PEM)"
              htmlFor={`${id}-ca`}
              help={storedCa ? "Paste a certificate to replace the stored one. The stored one is never shown." : "A private CA the service's certificate chains to; optional."}
            >
              <textarea id={`${id}-ca`} className="input mono" rows={4} spellCheck={false} value={caPem}
                onChange={(e) => { setCaPem(e.target.value); if (e.target.value) setRemoveCa(false); }} />
            </FormField>
          </div>
        </div>
      )}

      <FormField
        label="Maximum gRPC call duration (seconds)"
        htmlFor={`${id}-max`}
        w="sm"
        help="Ends a gRPC call that runs longer. 0 is no cap; at most 86400."
      >
        <Input id={`${id}-max`} type="number" min={0} max={86400} value={maxDuration} onChange={(e) => setMaxDuration(e.target.value)} />
      </FormField>

      <dl className="def-list">
        {data.client_upstream_protocol !== "" && (
          <div className="def-row">
            <dt className="def-key">Set by the client</dt>
            <dd className="def-val mono">{data.client_upstream_protocol}</dd>
          </div>
        )}
        <div className="def-row">
          <dt className="def-key">In effect</dt>
          <dd className="def-val mono">{data.effective_upstream_protocol}</dd>
        </div>
      </dl>
      {data.client_upstream_protocol !== "" && (
        <p className="muted small">A protocol the client names (<code>burrow grpc</code>, <code>--upstream-protocol</code>) overrides the choice above while it is connected.</p>
      )}

      <div className="col gap-1">
        {accessMode === "burrow_login" && (
          <p className="muted small">
            burrow_login is for browsers: a gRPC client cannot sign in on a page and gets UNAUTHENTICATED. Use API keys for a gRPC service.
          </p>
        )}
        <p className="muted small">gRPC calls are not recorded by the inspector; they appear in Traffic with their method and status.</p>
        <p className="muted small">
          gRPC calls are refused (PERMISSION_DENIED) while AI controls are on for the service: redaction, guardrails, routing or Anthropic translation.
        </p>
        {h2cOff && (
          <p className="muted small">
            This relay does not accept h2c from visitors (BURROW_HTTP_H2C is off): gRPC reaches it over TLS only.
          </p>
        )}
      </div>

      {err && <ErrorNotice role="alert">{err}</ErrorNotice>}
      <div className="panel-actions">
        <Button type="button" variant="primary" size="sm" disabled={save.isPending} onClick={() => { setErr(null); save.mutate(); }}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
    </>
  );
}
