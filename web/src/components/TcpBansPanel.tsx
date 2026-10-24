import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, Input } from "@/components/ds";
import { RelativeTime } from "@/components/RelativeTime";
import type { TcpBan } from "@/lib/contract";

const REFRESH_MS = 15_000;

/** The active bans of a tcp service, the form that bans a source by hand, and unban with a confirmation. */
export function TcpBansPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const titleId = useId();
  const key = ["service", serviceId, "tcp-bans"];
  const bans = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<TcpBan[]>(`/services/${serviceId}/tcp-bans`),
    refetchInterval: REFRESH_MS,
    refetchIntervalInBackground: false,
    retry: false,
  });
  const [source, setSource] = useState("");
  const [minutes, setMinutes] = useState("60");
  const [note, setNote] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [lifting, setLifting] = useState<TcpBan | null>(null);
  const [liftErr, setLiftErr] = useState<string | null>(null);

  const add = useMutation({
    mutationFn: () =>
      apiFetch<TcpBan>(`/services/${serviceId}/tcp-bans`, {
        method: "POST",
        body: JSON.stringify({ source: source.trim(), ban_min: Number(minutes), ...(note.trim() ? { note: note.trim() } : {}) }),
      }),
    onSuccess: async () => {
      setErr(null);
      setSource("");
      setNote("");
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't ban the source."),
  });

  const lift = useMutation({
    mutationFn: (s: string) =>
      apiFetch<void>(`/services/${serviceId}/tcp-bans?source=${encodeURIComponent(s)}`, { method: "DELETE" }),
    onSuccess: async () => {
      setLifting(null);
      setLiftErr(null);
      await qc.invalidateQueries({ queryKey: key });
    },
    onError: (e: unknown) => setLiftErr(e instanceof ApiError ? e.message : "Couldn't unban the source."),
  });

  const rows = bans.data ?? [];
  const minutesOk = Number.isInteger(Number(minutes)) && Number(minutes) >= 1 && Number(minutes) <= 43200;

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Bans</h2>
      <p className="muted small">
        A banned source is closed out before your machine hears of it. Bans end by themselves and survive a restart of the relay.
      </p>
      {bans.error ? (
        <ErrorNotice role="alert">Couldn&apos;t load the bans.</ErrorNotice>
      ) : rows.length === 0 ? (
        !bans.isLoading && <p className="muted small">No source is banned.</p>
      ) : (
        <div className="table-wrap">
          <table className="table" aria-label="Banned sources">
            <thead>
              <tr><th>Source</th><th>Banned</th><th>Until</th><th>Reason</th><th className="col-actions"></th></tr>
            </thead>
            <tbody>
              {rows.map((b) => (
                <tr key={b.source}>
                  <td className="mono">{b.source}</td>
                  <td>{b.auto ? "automatic" : "manual"}</td>
                  <td><RelativeTime value={b.until} /></td>
                  <td>{b.reason.replace(/^(auto|manual)(: )?/, "") || "—"}</td>
                  <td className="col-actions">
                    <Button type="button" variant="secondary" size="sm" aria-label={`Unban ${b.source}`}
                      onClick={() => { setLiftErr(null); setLifting(b); }}>
                      Unban
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="form-field-group">
        <FormField label="Source address" htmlFor={`${titleId}-source`} w="md" error={err ?? undefined}
          help="An IP address, or an IPv6 prefix of /64 or longer.">
          <Input id={`${titleId}-source`} mono value={source} autoComplete="off" placeholder="203.0.113.9" invalid={err !== null}
            onChange={(e) => { setSource(e.target.value); setErr(null); }} />
        </FormField>
        <FormField label="Ban for (minutes)" htmlFor={`${titleId}-minutes`} w="sm" help="1 to 43200 (30 days).">
          <Input id={`${titleId}-minutes`} type="number" min={1} max={43200} step={1} value={minutes}
            onChange={(e) => { setMinutes(e.target.value); setErr(null); }} />
        </FormField>
        <FormField label="Note" htmlFor={`${titleId}-note`} w="md">
          <Input id={`${titleId}-note`} value={note} maxLength={200} autoComplete="off" onChange={(e) => setNote(e.target.value)} />
        </FormField>
      </div>
      <div className="panel-actions">
        <Button type="button" variant="secondary" size="sm" disabled={add.isPending || source.trim() === "" || !minutesOk}
          onClick={() => { setErr(null); add.mutate(); }}>
          {add.isPending ? "Banning…" : "Ban source"}
        </Button>
      </div>

      <Dialog
        open={lifting !== null}
        onOpenChange={(o) => { if (!lift.isPending && !o) setLifting(null); }}
        size="sm"
        title={`Unban · ${lifting?.source ?? ""}`}
        description={`Let ${lifting?.source ?? "this source"} connect again?`}
        footer={
          <>
            <Button type="button" variant="secondary" disabled={lift.isPending} onClick={() => setLifting(null)}>Cancel</Button>
            <Button type="button" variant="destructive-solid" disabled={lift.isPending} onClick={() => lifting && lift.mutate(lifting.source)}>
              {lift.isPending ? "Unbanning…" : "Unban"}
            </Button>
          </>
        }
      >
        <p className="muted small">
          {lifting?.auto
            ? "The relay banned this source by itself. It will be banned again if it keeps connecting and leaving."
            : "The source can connect again at once, if the access rules let it in."}
        </p>
        {liftErr && <ErrorNotice role="alert">{liftErr}</ErrorNotice>}
      </Dialog>
    </section>
  );
}
