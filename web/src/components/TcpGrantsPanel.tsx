import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, ErrorNotice, Input } from "@/components/ds";
import { RelativeTime } from "@/components/RelativeTime";
import { remaining, widenCidr } from "@/lib/tcpAccess";
import type { IpGrant, IpGrantCreated } from "@/lib/contract";

const PRESETS = [1, 8, 24] as const;
const FAMILY: Record<IpGrant["family"], string> = { ipv4: "IPv4", ipv6: "IPv6" };
// The list is asked for again by itself, so a grant that ran out leaves it.
const REFRESH_MS = 30_000;

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

  const grants = useQuery({
    queryKey: ["service", serviceId, "ip-grants"],
    queryFn: () => apiFetch<IpGrant[]>(`/services/${serviceId}/ip-grants`),
    refetchInterval: REFRESH_MS,
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
      void qc.invalidateQueries({ queryKey: ["service", serviceId, "ip-grants"] });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't revoke the grant."),
  });

  function submitManual() {
    const cidr = manual.trim();
    if (!cidr) { setErr("Enter an address or range."); return; }
    create.mutate({ cidr, hours: Number(manualHours) }, { onSuccess: () => setManual("") });
  }

  const wider = created ? widenCidr(created.grant.cidr) : null;
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

      {rows.length === 0 ? (
        <p className="muted small">No grants right now.</p>
      ) : (
        <ul aria-label="Active grants" className="col gap-2" style={{ listStyle: "none", padding: 0, margin: 0 }}>
          {rows.map((g) => (
            <li key={g.id} className="row row-center gap-3">
              <span className="mono">{g.cidr}</span>
              <span className="muted small">{FAMILY[g.family]}</span>
              <span className="muted small">{remaining(g.expires_at)}</span>
              {g.note && <span className="muted small">{g.note}</span>}
              <Button
                type="button"
                variant="secondary"
                size="sm"
                disabled={revoke.isPending}
                aria-label={`Revoke ${g.cidr}`}
                onClick={() => revoke.mutate(g.id)}
              >
                Revoke
              </Button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
