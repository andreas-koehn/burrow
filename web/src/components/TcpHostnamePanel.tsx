import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { Button, ErrorNotice, FormField, Input } from "@/components/ds";
import { CopyableCode } from "@/components/CopyableCode";
import type { TcpAccessMode, TcpHostname } from "@/lib/contract";

/**
 * The host names that reach a tcp service through the relay's TLS passthrough listener: the one
 * the slug gives (shown to everyone who may configure the service) and a custom one, which only a
 * holder of services:configure:any may set.
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
            </>
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
