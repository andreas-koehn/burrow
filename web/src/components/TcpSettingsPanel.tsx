import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, ErrorNotice, FormField, Input, Switch } from "@/components/ds";
import type { TcpSettings } from "@/lib/contract";

/**
 * The settings that shape the connections of a tcp service: the PROXY protocol header its client
 * writes to the target (so that the target sees the visitor's address) and the idle timeout.
 * Saved together; only what changed is sent.
 */
export function TcpSettingsPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const titleId = useId();
  const key = ["service", serviceId, "tcp-settings"];
  const settings = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<TcpSettings>(`/services/${serviceId}/tcp-settings`),
    retry: false,
  });
  // null: the field shows what the relay has.
  const [proxy, setProxy] = useState<boolean | null>(null);
  const [idle, setIdle] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const data = settings.data;
  const savedProxy = data?.proxy_protocol === "v2";
  const savedIdleMin = data ? data.idle_timeout_s / 60 : 0;
  const shownProxy = proxy ?? savedProxy;
  const shownIdle = idle ?? String(savedIdleMin);
  const idleNumber = shownIdle.trim() === "" ? 0 : Number(shownIdle);
  const proxyChanged = proxy !== null && proxy !== savedProxy;
  const idleChanged = idle !== null && idleNumber !== savedIdleMin;

  const save = useMutation({
    mutationFn: () => {
      const body: { proxy_protocol?: string; idle_timeout_s?: number } = {};
      if (proxyChanged) body.proxy_protocol = shownProxy ? "v2" : "";
      if (idleChanged) body.idle_timeout_s = Math.round(idleNumber * 60);
      return apiFetch<TcpSettings>(`/services/${serviceId}/tcp-settings`, { method: "PUT", body: JSON.stringify(body) });
    },
    onSuccess: async () => {
      setErr(null);
      setProxy(null);
      setIdle(null);
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the settings."),
  });

  const idleInvalid = !Number.isFinite(idleNumber) || idleNumber < 0;
  const unsupported = data?.client_proxy_supported === false && shownProxy;

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Connection settings</h2>
      {settings.error ? (
        <ErrorNotice role="alert">Couldn&apos;t load the settings.</ErrorNotice>
      ) : (
        <>
          <div className="col gap-2">
            <label className="row row-center gap-2">
              <Switch aria-label="PROXY protocol v2" checked={shownProxy} disabled={!data} onChange={setProxy} />
              <span>Send a PROXY protocol v2 header to the target</span>
            </label>
            <p className="muted small">
              The client writes the header before the first byte, so a target that expects it (nginx, HAProxy, Postgres
              behind a proxy) sees the visitor&apos;s address instead of the client&apos;s. Turn it on only for a target that
              reads it: any other target takes the header for garbage.
              {data?.client_proxy_protocol === "v2" && " The client also sends it by its own configuration, whatever this switch says."}
            </p>
            {unsupported && (
              <ErrorNotice variant="warn" role="status">
                The connected client is too old to send the PROXY header. While this is on, the relay refuses its
                connections: update the client first.
              </ErrorNotice>
            )}
          </div>
          <FormField
            label="Idle timeout (minutes)"
            htmlFor={`${titleId}-idle`}
            w="sm"
            descId={`${titleId}-idle-desc`}
            error={err ?? undefined}
            help="A connection with no bytes in either direction for this long is closed. 0 is never; otherwise 1 to 1440."
          >
            <Input
              id={`${titleId}-idle`}
              type="number"
              min={0}
              max={1440}
              step={1}
              value={shownIdle}
              disabled={!data}
              invalid={idleInvalid || err !== null}
              aria-describedby={`${titleId}-idle-desc`}
              onChange={(e) => { setIdle(e.target.value); setErr(null); }}
            />
          </FormField>
          <div className="panel-actions">
            <Button
              type="button"
              variant="primary"
              size="sm"
              disabled={save.isPending || idleInvalid || (!proxyChanged && !idleChanged)}
              onClick={() => { setErr(null); save.mutate(); }}
            >
              {save.isPending ? "Saving…" : "Save settings"}
            </Button>
          </div>
        </>
      )}
    </section>
  );
}
