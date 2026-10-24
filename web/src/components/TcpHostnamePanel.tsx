import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { Button, ErrorNotice, FormField, Input, Switch } from "@/components/ds";
import { CopyableCode } from "@/components/CopyableCode";
import type { TcpAccessMode, TcpHostname } from "@/lib/contract";

// A listener as the card says it: the 443 mux is ":443", the dedicated one by its address.
function listenerText(l: string): string {
  return l === "https" ? ":443" : l === "passthrough_listener" ? "the passthrough listener" : l;
}

/**
 * The host names that reach a tcp service through the relay's TLS passthrough listener: the one
 * the slug gives (shown to everyone who may configure the service) and a custom one, which only a
 * holder of services:configure:any may set. With a name that reaches it, the service can be reached
 * by host name only: the relay then opens no public port for it.
 */
export function TcpHostnamePanel({ serviceId, mode }: { serviceId: string; mode: TcpAccessMode }) {
  const qc = useQueryClient();
  const titleId = useId();
  const canCustom = useCan(P.servicesConfigureAny);
  const key = ["service", serviceId, "tcp-hostname"];
  const host = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<TcpHostname>(`/services/${serviceId}/tcp-hostname`),
    retry: false,
  });
  const [draft, setDraft] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const data = host.data;
  const saved = data?.tcp_hostname ?? "";
  const shown = draft ?? saved;

  const save = useMutation({
    mutationFn: (name: string) =>
      apiFetch<TcpHostname>(`/services/${serviceId}/tcp-hostname`, { method: "PUT", body: JSON.stringify({ tcp_hostname: name }) }),
    onSuccess: async () => {
      setErr(null);
      setDraft(null);
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the host name."),
  });
  const [onlyErr, setOnlyErr] = useState<string | null>(null);
  const only = useMutation({
    mutationFn: (on: boolean) =>
      apiFetch<TcpHostname>(`/services/${serviceId}/tcp-hostname`, { method: "PUT", body: JSON.stringify({ hostname_only: on }) }),
    onSuccess: async () => {
      setOnlyErr(null);
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setOnlyErr(e instanceof ApiError ? e.message : "Couldn't change how the service is reached."),
  });
  // The port may go only while a name reaches the service; it may always come back. `hostnames` is the
  // relay's own rule (a legacy custom name under the tcp domain is not in it), so this matches the relay.
  const canOnly = !!data && data.passthrough && data.hostnames.length > 0;
  const showOnly = !!data && ((data.passthrough && mode !== "private") || data.hostname_only);

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Host name</h2>
      {host.error ? (
        <ErrorNotice role="alert">Couldn&apos;t load the host names.</ErrorNotice>
      ) : !data ? null : (
        <>
          {!data.passthrough && (
            <p className="muted small">
              This relay has no TLS passthrough listener, so a service is reached by its port only.
            </p>
          )}
          {mode === "private" && (
            <p className="muted small">
              A private service has no name on the passthrough listener. Choose Open or IP allowlist to give it one.
            </p>
          )}
          {data.passthrough && mode !== "private" && (
            <>
              <p className="muted small">
                A client that speaks TLS and names one of these hosts reaches the service through the relay without a port.
                The relay does not terminate TLS: the target serves its own certificate.
              </p>
              {data.hostnames.length === 0
                ? <p className="muted small">No name is set: the relay has no domain for slug names.</p>
                : data.hostnames.map((h) => <CopyableCode key={h} text={h} label={`Copy ${h}`} />)}
              {/* Where those names reach the service: the relay's HTTPS port (the SNI mux) and/or its own listener. */}
              {data.reachable_on !== undefined && data.reachable_on.length > 0 && (
                <p className="small">Reachable on {data.reachable_on.map(listenerText).join(" and ")}.</p>
              )}
              {data.shadowed_by !== undefined && (
                <ErrorNotice variant="warn" role="alert">
                  {data.shadowed_by === "custom_domain"
                    ? "A custom domain of an http service covers this name, so the relay answers it there: it is not reached on :443 as a TLS passthrough name. Remove the custom domain or use another name."
                    : "The relay serves this name itself on :443 (its certificate, ACME or the dashboard's host), so it is not reached on :443 as a TLS passthrough name. Use another name."}
                </ErrorNotice>
              )}
            </>
          )}
          {showOnly && data && (
            <div className="col gap-1">
              <label className="row row-center gap-2">
                <Switch
                  aria-label="Host name only"
                  aria-describedby={`${titleId}-only-desc`}
                  checked={data.hostname_only}
                  disabled={only.isPending || (!data.hostname_only && !canOnly)}
                  onChange={(on) => { setOnlyErr(null); only.mutate(on); }}
                />
                <span>Reach it by host name only</span>
              </label>
              <p id={`${titleId}-only-desc`} className="muted small">
                {data.hostname_only && !canOnly
                  ? `Host name only is on, but the public port is open again: ${
                    !data.passthrough
                      ? "this relay has no TLS passthrough listener, so no name reaches the service."
                      : "the service has no host name on the passthrough listener (set a custom name, or ask the operator for a domain for slug names)."
                  } The relay never leaves a service without a way in; turn this off, or give it a name.`
                  : data.hostname_only
                  ? "The service has no public port: it is reached by its host names only. The client reconnects on its own when this changes."
                  : canOnly
                    ? "Turn this on to close the service's public port, so that only a client that names the host reaches it. The client reconnects on its own."
                    : "Host name only needs a host name that reaches the service: set a custom name, or ask the operator for a domain for slug names."}
              </p>
              {onlyErr && <ErrorNotice role="alert">{onlyErr}</ErrorNotice>}
            </div>
          )}
          {canCustom ? (
            <>
              <FormField
                label="Custom host name"
                htmlFor={`${titleId}-name`}
                w="md"
                descId={`${titleId}-name-desc`}
                error={err ?? undefined}
                help="A name you point at the relay with DNS, such as rdp.example.org. One service per name."
              >
                <Input
                  id={`${titleId}-name`}
                  mono
                  value={shown}
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                  invalid={err !== null}
                  aria-describedby={`${titleId}-name-desc`}
                  onChange={(e) => { setDraft(e.target.value); setErr(null); }}
                />
              </FormField>
              <div className="panel-actions">
                <Button
                  type="button"
                  variant="primary"
                  size="sm"
                  disabled={save.isPending || draft === null || draft.trim() === saved}
                  onClick={() => { setErr(null); save.mutate(shown.trim()); }}
                >
                  {save.isPending ? "Saving…" : "Save host name"}
                </Button>
                {saved !== "" && (
                  <Button type="button" variant="secondary" size="sm" disabled={save.isPending} onClick={() => { setErr(null); save.mutate(""); }}>
                    Remove custom name
                  </Button>
                )}
              </div>
            </>
          ) : (
            <p className="muted small">A custom host name needs the services:configure:any permission.</p>
          )}
        </>
      )}
    </section>
  );
}
