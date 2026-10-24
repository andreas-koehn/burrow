import { useId, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { AccessModeCard, Button, ErrorNotice } from "@/components/ds";
import { AccessPolicyEditor } from "@/components/AccessPolicyEditor";
import { CopyableCode } from "@/components/CopyableCode";
import { IPGeoPanel } from "@/components/IPGeoPanel";
import { TcpConnectionsPanel } from "@/components/TcpConnectionsPanel";
import { TcpLiveConnectionsPanel } from "@/components/TcpLiveConnectionsPanel";
import { TcpGrantsPanel } from "@/components/TcpGrantsPanel";
import { TcpSettingsPanel } from "@/components/TcpSettingsPanel";
import { TcpHostnamePanel } from "@/components/TcpHostnamePanel";
import { TcpCountriesPanel } from "@/components/TcpCountriesPanel";
import { TcpLimitsPanel } from "@/components/TcpLimitsPanel";
import { TcpBansPanel } from "@/components/TcpBansPanel";
import { useRelayHost } from "@/lib/useRelayHost";
import { ACCESS_LABEL, MODES, FORWARD_LOCAL_PORT, RISKY_PORTS, forwardCommand, restrictiveness, riskyPort, localPort } from "@/lib/tcpAccess";
import type { Service, TcpAccessMode } from "@/lib/contract";

function asTcpMode(m: Service["access_mode"]): TcpAccessMode {
  return m === "ip" || m === "private" ? m : "open";
}

/**
 * The page body of a tcp service: its access mode, how to connect, who gets in
 * (addresses and grants in IP mode, roles for a forward) and what came through.
 */
export function TcpAccessPanel({ service }: { service: Service }) {
  const qc = useQueryClient();
  const modeTitleId = useId();
  const saved = asTcpMode(service.access_mode);
  const [selected, setSelected] = useState<TcpAccessMode>(saved);
  const [err, setErr] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/services/${service.id}/access-mode`, { method: "PUT", body: JSON.stringify({ access_mode: selected }) }),
    onSuccess: async () => {
      setErr(null);
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["service", service.id] }),
        qc.invalidateQueries({ queryKey: ["services"] }),
        qc.invalidateQueries({ queryKey: ["tunnels"] }),
      ]);
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't save the access mode."),
  });

  const risky = riskyPort(service.local_addr);
  // The relay checks both on every write; the page does not offer what would be refused.
  const canConfigure = useCan([P.servicesConfigureOwn, P.servicesConfigureAny]);
  const canAddresses = useCan([P.ipgeoManageOwn, P.ipgeoManageAny]);
  // A new mode closes the serving client's session and it registers again. A client started
  // without --access serves the new mode; one started with a stricter --access stops.
  const stricter = restrictiveness(selected) > restrictiveness(saved);

  return (
    <div className="col gap-4">
      <section className="card col gap-3" aria-labelledby={modeTitleId}>
        <h2 id={modeTitleId}>Access mode</h2>
        {!canConfigure ? (
          <p>
            <span className="muted">Mode: </span>{ACCESS_LABEL[saved]}.{" "}
            <span className="muted">Changing the mode needs the services:configure permission.</span>
          </p>
        ) : (<>
        <div role="radiogroup" aria-label="Access mode" className="mode-list">
          {MODES.map((m) => (
            <AccessModeCard
              key={m.value}
              title={m.title}
              description={m.help}
              selected={selected === m.value}
              onSelect={() => { setSelected(m.value); setErr(null); }}
            />
          ))}
        </div>
        {selected === "open" && (
          <ErrorNotice>
            Anyone on the internet can connect to this port, and Burrow adds no authentication: the
            service has to protect itself.
            {risky !== null && (
              <> Port {risky} ({RISKY_PORTS[risky]}) is one that attackers scan for all day. Prefer Private or IP allowlist.</>
            )}
          </ErrorNotice>
        )}
        {selected !== saved && stricter && (
          <p className="muted small">A stricter mode applies when the client reconnects, which it does by itself.</p>
        )}
        {selected !== saved && !stricter && (
          <ErrorNotice role="status" title="This makes the service more open">
            Clients started without --access serve {selected} when they reconnect, which they do by
            themselves. A client started with a stricter --access stops rather than serve a more open mode.
          </ErrorNotice>
        )}
        {err && <p role="alert" className="notice-inline">{err}</p>}
        <div className="panel-actions">
          <Button
            type="button"
            variant="primary"
            size="sm"
            disabled={save.isPending || selected === saved}
            onClick={() => save.mutate()}
          >
            {save.isPending ? "Saving…" : "Save mode"}
          </Button>
        </div>
        </>)}
      </section>

      <ConnectCard service={service} mode={saved} />

      {canConfigure && <TcpHostnamePanel serviceId={service.id} mode={saved} />}
      {canConfigure && <TcpSettingsPanel serviceId={service.id} />}

      {saved === "ip" && canAddresses && <IPGeoPanel serviceId={service.id} tcp />}
      {saved !== "private" && canAddresses && <TcpCountriesPanel serviceId={service.id} />}
      {saved === "ip" && canAddresses && <TcpGrantsPanel serviceId={service.id} />}
      {saved === "ip" && !canAddresses && (
        <p className="muted small">Managing the allowed addresses and grants needs the ipgeo:manage permission.</p>
      )}
      {saved === "open" && !canAddresses && (
        <p className="muted small">Managing the country rules needs the ipgeo:manage permission.</p>
      )}

      {canConfigure && (
        <section className="card col gap-2" aria-label="Who may forward">
          <AccessPolicyEditor serviceId={service.id} forward />
        </section>
      )}

      {canConfigure && <TcpLimitsPanel serviceId={service.id} />}
      {canConfigure && <TcpBansPanel serviceId={service.id} />}

      {canConfigure && <TcpLiveConnectionsPanel serviceId={service.id} />}
      <TcpConnectionsPanel serviceId={service.id} />
    </div>
  );
}

function ConnectCard({ service, mode }: { service: Service; mode: TcpAccessMode }) {
  const titleId = useId();
  const host = useRelayHost(mode !== "private");
  // The slug is the global target of `burrow forward`; a service without one is reached by its name.
  const target = service.slug || service.name || service.id;
  const rdp = localPort(service.local_addr) === 3389;
  const address = service.remote_port > 0 ? `${host}:${service.remote_port}` : null;

  return (
    <section className="card col gap-2" aria-labelledby={titleId}>
      <h2 id={titleId}>Connect</h2>
      {mode === "private" ? (
        <>
          <p className="muted small">
            This service has no public port. On the machine you connect from, signed in to Burrow, run:
          </p>
          <CopyableCode text={forwardCommand(target)} label="Copy the forward command" />
          {rdp ? (
            <p className="muted small">
              Remote Desktop: start <code>mstsc</code> and connect to <code>localhost:{FORWARD_LOCAL_PORT}</code>.
              Any other client connects to that address the same way.
            </p>
          ) : (
            <p className="muted small">
              Then connect your client to <code>localhost:{FORWARD_LOCAL_PORT}</code>.
            </p>
          )}
        </>
      ) : address ? (
        <>
          <p className="muted small">
            {mode === "ip" ? "Allowed addresses connect to:" : "Visitors connect to:"}
          </p>
          <CopyableCode text={address} label={`Copy address ${address}`} />
          {rdp && <p className="muted small">Remote Desktop: enter this address in <code>mstsc</code>.</p>}
        </>
      ) : (
        <p className="muted small">The public address appears once a client serves the service.</p>
      )}
    </section>
  );
}
