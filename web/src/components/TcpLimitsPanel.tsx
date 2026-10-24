import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, ErrorNotice, FormField, Input, Switch } from "@/components/ds";
import { formatBytes } from "@/lib/format";
import type { TcpLimits } from "@/lib/contract";

const GIB = 1024 ** 3;
const AUTOBAN_FIELDS = [
  { key: "short_s", label: "Shorter than (seconds)", help: "A connection that ends within this time counts as short.", min: 1, max: 300 },
  { key: "count", label: "Connections", help: "Short connections of one source in the window that ban it.", min: 2, max: 100 },
  { key: "window_min", label: "Window (minutes)", help: "How far back the short connections are counted.", min: 1, max: 1440 },
  { key: "ban_min", label: "Ban for (minutes)", help: "How long the source stays banned.", min: 1, max: 43200 },
] as const;
type AutobanKey = (typeof AUTOBAN_FIELDS)[number]["key"];

/**
 * The limits of a tcp service: the bandwidth of each direction, the volume per UTC day (with today's
 * usage) and the automatic bans. One save sends all three; the relay asks for the password again.
 */
export function TcpLimitsPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const titleId = useId();
  const key = ["service", serviceId, "tcp-limits"];
  const limits = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<TcpLimits>(`/services/${serviceId}/tcp-limits`),
    retry: false,
  });
  const [kbps, setKbps] = useState<string | null>(null);
  const [gib, setGib] = useState<string | null>(null);
  const [ban, setBan] = useState<{ enabled?: boolean } & Partial<Record<AutobanKey, string>>>({});
  const [err, setErr] = useState<string | null>(null);

  const data = limits.data;
  const shownKbps = kbps ?? String(data?.rate_limit_kbps || "");
  const shownGib = gib ?? (data && data.daily_bytes > 0 ? String(data.daily_bytes / GIB) : "");
  const enabled = ban.enabled ?? data?.autoban.enabled ?? false;
  const shownBan = (k: AutobanKey) => ban[k] ?? String(data?.autoban[k] ?? "");

  const num = (t: string) => (t.trim() === "" ? 0 : Number(t));
  const kbpsN = num(shownKbps);
  const gibN = num(shownGib);
  const invalid = !Number.isFinite(kbpsN) || kbpsN < 0 || !Number.isFinite(gibN) || gibN < 0
    || AUTOBAN_FIELDS.some((f) => !Number.isInteger(num(shownBan(f.key))));
  const dirty = kbps !== null || gib !== null || Object.keys(ban).length > 0;

  const save = useMutation({
    mutationFn: () =>
      apiFetch<TcpLimits>(`/services/${serviceId}/tcp-limits`, {
        method: "PUT",
        body: JSON.stringify({
          rate_limit_kbps: Math.round(kbpsN),
          daily_bytes: Math.round(gibN * GIB),
          autoban: {
            enabled,
            short_s: num(shownBan("short_s")), count: num(shownBan("count")),
            window_min: num(shownBan("window_min")), ban_min: num(shownBan("ban_min")),
          },
        }),
      }),
    onSuccess: async () => {
      setErr(null);
      setKbps(null);
      setGib(null);
      setBan({});
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the limits."),
  });

  const usage = data?.usage_today;
  const pct = data && data.daily_bytes > 0 && usage ? Math.min(100, Math.round((usage.bytes / data.daily_bytes) * 100)) : null;

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Limits</h2>
      {limits.error ? (
        <ErrorNotice role="alert">Couldn&apos;t load the limits.</ErrorNotice>
      ) : (
        <>
          <FormField
            label="Bandwidth limit (kbit/s)"
            htmlFor={`${titleId}-kbps`}
            w="sm"
            help="For each direction, shared by all connections of the service. Empty is no limit; otherwise 64 to 10000000."
          >
            <Input id={`${titleId}-kbps`} type="number" min={0} step={1} value={shownKbps} disabled={!data}
              onChange={(e) => { setKbps(e.target.value); setErr(null); }} />
          </FormField>
          <FormField
            label="Daily volume (GiB)"
            htmlFor={`${titleId}-gib`}
            w="sm"
            help="Bytes in and out per UTC day. Over it, new visitors are refused and open ones closed; forwards count towards it but keep working. Empty is no limit."
          >
            <Input id={`${titleId}-gib`} type="number" min={0} step="any" value={shownGib} disabled={!data}
              onChange={(e) => { setGib(e.target.value); setErr(null); }} />
          </FormField>
          {pct !== null && data && usage && (
            <div className="col gap-1">
              <div
                className="usage-bar"
                role="progressbar"
                aria-label="Today's volume"
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={pct}
                data-level={pct >= 100 ? "full" : pct >= 80 ? "warn" : "ok"}
              >
                <span style={{ width: `${pct}%` }} />
              </div>
              <p className="muted small">
                {formatBytes(usage.bytes)} of {formatBytes(data.daily_bytes)} today ({usage.day}, UTC): {formatBytes(usage.bytes_in)} in, {formatBytes(usage.bytes_out)} out.
              </p>
            </div>
          )}
          {data?.quota_reached && (
            <ErrorNotice variant="warn" role="status">
              The daily volume is used up: new visitors are refused until the next UTC day. Forwards still connect.
            </ErrorNotice>
          )}

          <div className="col gap-2">
            <label className="row row-center gap-2">
              <Switch aria-label="Automatic bans" checked={enabled} disabled={!data} onChange={(on) => { setBan((b) => ({ ...b, enabled: on })); setErr(null); }} />
              <span>Ban sources that connect and leave again at once</span>
            </label>
            <p className="muted small">
              A source whose connections end within a few seconds, again and again, is a scanner or a broken client.
              The relay closes it out for a while. Forwards are never banned.
            </p>
            <div className="form-field-group">
              {AUTOBAN_FIELDS.map((f) => (
                <FormField key={f.key} label={f.label} htmlFor={`${titleId}-${f.key}`} w="sm" help={f.help}>
                  <Input id={`${titleId}-${f.key}`} type="number" min={f.min} max={f.max} step={1} value={shownBan(f.key)} disabled={!data}
                    onChange={(e) => { setBan((b) => ({ ...b, [f.key]: e.target.value })); setErr(null); }} />
                </FormField>
              ))}
            </div>
          </div>

          {err && <ErrorNotice role="alert">{err}</ErrorNotice>}
          <div className="panel-actions">
            <Button type="button" variant="primary" size="sm" disabled={save.isPending || invalid || !dirty}
              onClick={() => { setErr(null); save.mutate(); }}>
              {save.isPending ? "Saving…" : "Save limits"}
            </Button>
          </div>
        </>
      )}
    </section>
  );
}
