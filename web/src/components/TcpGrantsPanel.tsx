import { useEffect, useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, Input } from "@/components/ds";
import { RelativeTime } from "@/components/RelativeTime";
import { remaining, widenCidr } from "@/lib/tcpAccess";
import type { IpGrant, IpGrantCreated } from "@/lib/contract";

const PRESETS = [1, 8, 24] as const;
const FAMILY: Record<IpGrant["family"], string> = { ipv4: "IPv4", ipv6: "IPv6" };
// The list is asked for again by itself, so a grant that ran out leaves it.
const REFRESH_MS = 30_000;

/** The clock, moved on every minute so a time left counts down. */
function useMinuteClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(t);
  }, []);
  return now;
}

/**
 * Just-in-time access for a tcp service in IP allowlist mode: the caller's own
 * address for a few hours, a wider range, or an address typed in, each until a
 * time. Grants that are still valid are listed with their time left and can be revoked.
 */
export function TcpGrantsPanel({ serviceId }: { serviceId: string }) {
  const qc = useQueryClient();
  const titleId = useId();
  const manualId = useId();
  const hoursId = useId();
  const [created, setCreated] = useState<{ grant: IpGrantCreated; hours: number } | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [manual, setManual] = useState("");
  const [manualHours, setManualHours] = useState("8");
  const [revoking, setRevoking] = useState<IpGrant | null>(null);
  const now = useMinuteClock();

  const grants = useQuery({
    queryKey: ["service", serviceId, "ip-grants"],
    queryFn: () => apiFetch<IpGrant[]>(`/services/${serviceId}/ip-grants`),
    refetchInterval: REFRESH_MS,
    // A hidden tab does not poll; the list is fetched again when the window regains focus.
    refetchIntervalInBackground: false,
    retry: false,
  });

  const create = useMutation({
    mutationFn: ({ cidr, hours }: { cidr?: string; hours: number }) =>
      apiFetch<IpGrantCreated>(`/services/${serviceId}/ip-grants`, {
        method: "POST",
        body: JSON.stringify(cidr ? { cidr, ttl_hours: hours } : { ttl_hours: hours }),
      }),
    onSuccess: (grant, vars) => {
      setErr(null);
      setCreated({ grant, hours: vars.hours });
      void qc.invalidateQueries({ queryKey: ["service", serviceId, "ip-grants"] });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't add the grant."),
  });

  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/services/${serviceId}/ip-grants/${encodeURIComponent(id)}`, { method: "DELETE" }),
    onSuccess: () => {
      setErr(null);
      setRevoking(null);
      void qc.invalidateQueries({ queryKey: ["service", serviceId, "ip-grants"] });
    },
    onError: (e: unknown) => {
      setRevoking(null);
      setErr(e instanceof ApiError ? e.message : "Couldn't revoke the grant.");
    },
  });

  function submitManual() {
    const cidr = manual.trim();
    if (!cidr) { setErr("Enter an address or range."); return; }
    create.mutate({ cidr, hours: Number(manualHours) }, { onSuccess: () => setManual("") });
  }

  // A private address is the relay's view of a proxy: a wider range would let in the proxy's whole network.
  const wider = created && !created.grant.warning ? widenCidr(created.grant.cidr) : null;
  const rows = grants.data ?? [];

  return (
    <section className="card col gap-3" aria-labelledby={titleId}>
      <h2 id={titleId}>Allow my address</h2>
      <p className="muted small">
        A grant lets an address connect for a while, on top of the permanent list. It ends by itself;
        connections already open stay.
      </p>
      <div className="row row-center gap-2 wrap">
        {PRESETS.map((h) => (
          <Button
            key={h}
            type="button"
            variant="secondary"
            size="sm"
            disabled={create.isPending}
            onClick={() => create.mutate({ hours: h })}
          >
            Allow my IP for {h} h
          </Button>
        ))}
      </div>

      {created && (
        <div role="status" className="col gap-2">
          <p>
            Allowed <span className="mono">{created.grant.cidr}</span> ({FAMILY[created.grant.family]}
            {created.grant.from_caller ? ", your address as the relay sees it" : ""}) until{" "}
            <RelativeTime value={created.grant.expires_at} />.
          </p>
          {created.grant.warning && <ErrorNotice variant="warn" role="note">{created.grant.warning}</ErrorNotice>}
          {created.grant.from_caller && wider && (
            <div className="row row-center gap-2">
              <span className="muted small">A home connection may change address:</span>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                disabled={create.isPending}
                onClick={() => create.mutate({ cidr: wider, hours: created.hours })}
              >
                Widen to {wider}
              </Button>
            </div>
          )}
        </div>
      )}

      <div className="row gap-2 wrap" style={{ alignItems: "flex-end" }}>
        <div className="form-field">
          <label htmlFor={manualId}>Another address</label>
          <Input
            id={manualId}
            className="mono"
            value={manual}
            onChange={(e) => setManual(e.target.value)}
            placeholder="198.51.100.4 or 2001:db8::/48"
          />
        </div>
        <div className="form-field">
          <label htmlFor={hoursId}>For</label>
          <select id={hoursId} className="input" value={manualHours} onChange={(e) => setManualHours(e.target.value)}>
            {PRESETS.map((h) => <option key={h} value={String(h)}>{h} {h === 1 ? "hour" : "hours"}</option>)}
          </select>
        </div>
        <Button type="button" variant="secondary" size="sm" disabled={create.isPending} onClick={submitManual}>
          Allow address
        </Button>
      </div>

      {err && <ErrorNotice role="alert">{err}</ErrorNotice>}

      {grants.error ? (
        <ErrorNotice role="alert">
          Couldn&apos;t load the grants.{grants.error instanceof ApiError && ` ${grants.error.message}`}
        </ErrorNotice>
      ) : rows.length === 0 ? (
        <p className="muted small">No grants right now.</p>
      ) : (
        <ul aria-label="Active grants" className="col gap-2" style={{ listStyle: "none", padding: 0, margin: 0 }}>
          {rows.map((g) => (
            <li key={g.id} className="row row-center gap-3">
              <span className="mono">{g.cidr}</span>
              <span className="muted small">{FAMILY[g.family]}</span>
              <span className="muted small">{remaining(g.expires_at, now)}</span>
              {g.note && <span className="muted small">{g.note}</span>}
              <Button
                type="button"
                variant="secondary"
                size="sm"
                aria-label={`Revoke ${g.cidr}`}
                onClick={() => setRevoking(g)}
              >
                Revoke
              </Button>
            </li>
          ))}
        </ul>
      )}
      <Dialog
        open={revoking !== null}
        onOpenChange={(o) => { if (!revoke.isPending && !o) setRevoking(null); }}
        size="sm"
        title={`Revoke grant · ${revoking?.cidr ?? ""}`}
        description={`Stop letting ${revoking?.cidr ?? "this range"} connect?`}
        footer={
          <>
            <Button type="button" variant="secondary" disabled={revoke.isPending} onClick={() => setRevoking(null)}>Cancel</Button>
            <Button
              type="button"
              variant="destructive-solid"
              disabled={revoke.isPending}
              onClick={() => revoking && revoke.mutate(revoking.id)}
            >
              {revoke.isPending ? "Revoking…" : "Revoke grant"}
            </Button>
          </>
        }
      >
        <p className="muted small">
          New connections from {revoking?.cidr} are refused at once. Connections already open stay.
        </p>
      </Dialog>
    </section>
  );
}
