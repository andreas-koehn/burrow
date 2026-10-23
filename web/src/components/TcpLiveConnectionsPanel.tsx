import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice } from "@/components/ds";
import { RelativeTime } from "@/components/RelativeTime";
import { formatBytes } from "@/lib/format";
import type { ServiceConnection } from "@/lib/contract";

const REFRESH_MS = 5_000;

/** Who a connection is: the visitor's address, or the person and token that forward. */
function who(c: ServiceConnection): string {
  return c.kind === "forward" ? c.user_email || c.user_id || c.source_ip : c.source_ip;
}

/**
 * The connections a tcp service carries now: visitors of its public port and forwards of signed-in
 * clients, with their bytes so far. A connection can be cut off; the visitor or the client may
 * connect again if the access rules still let them.
 */
export function TcpLiveConnectionsPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const titleId = useId();
  const [closing, setClosing] = useState<ServiceConnection | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const key = ["service", serviceId, "connections"];

  const conns = useQuery({
    queryKey: key,
    queryFn: () => apiFetch<ServiceConnection[]>(`/services/${serviceId}/connections`),
    refetchInterval: REFRESH_MS,
    retry: false,
  });

  const close = useMutation({
    mutationFn: (id: string) =>
      apiFetch<void>(`/services/${serviceId}/connections/${encodeURIComponent(id)}`, { method: "DELETE" }),
    onSuccess: () => {
      setClosing(null);
      void qc.invalidateQueries({ queryKey: key });
      void qc.invalidateQueries({ queryKey: ["connection-logs", "service", serviceId] });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't disconnect."),
  });

  const rows = conns.data ?? [];

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Live connections</h2>
      {conns.error ? (
        <ErrorNotice role="alert">
          Couldn&apos;t load the connections.{conns.error instanceof ApiError && ` ${conns.error.message}`}
        </ErrorNotice>
      ) : rows.length === 0 ? (
        !conns.isLoading && <p className="muted small">No connection is open right now.</p>
      ) : (
        <div className="table-wrap">
          <table className="table" aria-label="Open connections">
            <thead>
              <tr><th>Kind</th><th>Who</th><th>Since</th><th>Traffic</th><th className="col-actions"></th></tr>
            </thead>
            <tbody>
              {rows.map((c) => (
                <tr key={c.id}>
                  <td>{c.kind === "forward" ? "Forward" : "Visitor"}</td>
                  <td>
                    <span className="mono">{who(c)}</span>
                    {c.kind === "forward" && c.token_name && <span className="muted small"> · {c.token_name}</span>}
                  </td>
                  <td><RelativeTime value={c.since} /></td>
                  <td>
                    <span title={`In: ${c.bytes_in} bytes`}>↓ {formatBytes(c.bytes_in)}</span>{"  "}
                    <span title={`Out: ${c.bytes_out} bytes`}>↑ {formatBytes(c.bytes_out)}</span>
                  </td>
                  <td className="col-actions">
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                      aria-label={`Disconnect ${who(c)}`}
                      onClick={() => { setErr(null); setClosing(c); }}
                    >
                      Disconnect
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={closing !== null}
        onOpenChange={(o) => { if (!close.isPending && !o) setClosing(null); }}
        size="sm"
        title={`Disconnect · ${closing ? who(closing) : ""}`}
        description={`Close the connection of ${closing ? who(closing) : "this source"} now?`}
        footer={
          <>
            <Button type="button" variant="secondary" disabled={close.isPending} onClick={() => setClosing(null)}>Cancel</Button>
            <Button
              type="button"
              variant="destructive-solid"
              disabled={close.isPending}
              onClick={() => closing && close.mutate(closing.id)}
            >
              {close.isPending ? "Disconnecting…" : "Disconnect"}
            </Button>
          </>
        }
      >
        <p className="muted small">
          The connection ends at once and what is in flight is lost. {closing?.kind === "forward"
            ? "The client stays signed in and may open another forward while its role is allowed."
            : "The visitor may connect again while the access rules let them in."}
        </p>
        {err && <ErrorNotice role="alert">{err}</ErrorNotice>}
      </Dialog>
    </section>
  );
}
