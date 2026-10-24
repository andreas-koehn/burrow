import { useId, useState } from "react";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { AccessModeCard, Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input } from "@/components/ds";
import { CopyableCode } from "@/components/CopyableCode";
import { MODES, tcpClientCommand, tcpClientYaml } from "@/lib/tcpAccess";
import type { CreateTcpServiceInput, TcpAccessMode } from "@/lib/contract";

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

/**
 * Creates a tcp service before any client connects: its name, its access mode (private unless chosen
 * otherwise), optionally a fixed public port. Afterwards the dialog shows what to run on the machine
 * that holds the service. `kindSwitch` is the choice between a web and a tcp service, above the form.
 */
export function NewTcpServiceDialog({ open, onOpenChange, kindSwitch }: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  kindSwitch: ReactNode;
}) {
  const qc = useQueryClient();
  const modeTitleId = useId();
  const [name, setName] = useState("");
  const [mode, setMode] = useState<TcpAccessMode>("private");
  const [port, setPort] = useState("");
  const [localPort, setLocalPort] = useState("");
  const [nameLeft, setNameLeft] = useState(false);
  const [nameErr, setNameErr] = useState<string | null>(null);
  const [portErr, setPortErr] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [created, setCreated] = useState<{ id: string; name: string; mode: TcpAccessMode } | null>(null);

  const trimmed = name.trim();
  const nameProblem = trimmed === "" ? "Enter a name." : NAME_RE.test(trimmed) ? null : "Use letters, digits, dot, underscore and hyphen, up to 64.";
  const portNumber = port.trim() === "" ? null : Number(port);
  const portProblem = mode === "private" || portNumber === null ? null
    : !Number.isInteger(portNumber) || portNumber < 1 || portNumber > 65535 ? "Use a port between 1 and 65535." : null;

  function reset() {
    setName(""); setMode("private"); setPort(""); setLocalPort("");
    setNameLeft(false); setNameErr(null); setPortErr(null); setErr(null); setCreated(null);
  }
  function close() { onOpenChange(false); reset(); }

  const create = useMutation({
    mutationFn: () => {
      const body: CreateTcpServiceInput = { type: "tcp", name: trimmed, access_mode: mode };
      if (mode !== "private" && portNumber !== null) body.tcp_port = portNumber;
      return apiFetch<{ id: string }>("/services", { method: "POST", body: JSON.stringify(body) });
    },
    onSuccess: async (r) => {
      setCreated({ id: r.id, name: trimmed, mode });
      await qc.invalidateQueries({ queryKey: ["services"] });
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setErr("Couldn't create the service.");
      else if (e.status === 403) setErr("You don't have permission to create services.");
      else if (e.message.startsWith("tcp_port")) setPortErr(e.message);
      else if (e.status === 409 || e.message.startsWith("name")) setNameErr(e.message);
      else setErr(e.message);
    },
  });

  const nameError = nameErr ?? (nameLeft ? nameProblem : null);
  const blocked = nameProblem !== null || portProblem !== null;

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o) close(); }}
      title="New TCP service"
      description={created ? undefined : "Reserve a tcp service before a client serves it: RDP, SSH, a database."}
      footer={created ? (
        <Button variant="primary" onClick={close}>Done</Button>
      ) : (
        <>
          <Button variant="secondary" onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            disabled={blocked || create.isPending}
            onClick={() => { setErr(null); setNameErr(null); setPortErr(null); create.mutate(); }}
          >
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </>
      )}
    >
      {created ? (
        <div className="col gap-3">
          <p>
            <strong>{created.name}</strong> exists
            {created.mode === "private" ? ", with no public port: reach it with burrow forward." : created.mode === "ip" ? ", as an IP allowlist service." : ", open to the internet."}
            {" "}Run this on the machine that holds the service:
          </p>
          <CopyableCode text={tcpClientCommand(created.name, localPort)} label="Copy the client command" />
          <p className="muted small">Or add it to burrow.yaml and run <code>burrow up</code>:</p>
          <CopyableCode text={tcpClientYaml(created.name, localPort)} label="Copy the burrow.yaml entry" />
          <p className="muted small">
            The client serves the service in the mode set here. Its port, host name, limits and bans are on the service&apos;s page.
          </p>
          <p><Link className="link-inline" to={`/services/${encodeURIComponent(created.id)}`} onClick={close}>Open the service</Link></p>
        </div>
      ) : (
        <>
          {kindSwitch}
          <FormFieldGroup>
            <FormField
              label="Name"
              htmlFor="nt-name"
              w="md"
              descId="nt-name-desc"
              error={nameError ?? undefined}
              help="The name the client registers: burrow tcp 3389 --name <name>."
            >
              <Input
                id="nt-name"
                value={name}
                maxLength={64}
                autoComplete="off"
                autoCapitalize="none"
                invalid={nameError !== null}
                aria-describedby="nt-name-desc"
                onChange={(e) => { setName(e.target.value); setNameErr(null); }}
                onBlur={() => setNameLeft(true)}
              />
            </FormField>
            <div className="col gap-2" role="group" aria-labelledby={modeTitleId}>
              <span id={modeTitleId} className="field-label">Access mode</span>
              <div role="radiogroup" aria-label="Access mode" className="mode-list">
                {MODES.map((m) => (
                  <AccessModeCard
                    key={m.value}
                    title={m.title}
                    description={m.help}
                    selected={mode === m.value}
                    onSelect={() => { setMode(m.value); setPortErr(null); }}
                  />
                ))}
              </div>
            </div>
            {mode === "open" && (
              <ErrorNotice>
                Anyone on the internet can connect to this port, and Burrow adds no authentication: the service has to
                protect itself.
              </ErrorNotice>
            )}
            {mode !== "private" && (
              <FormField
                label="Public port"
                htmlFor="nt-port"
                w="sm"
                descId="nt-port-desc"
                error={portErr ?? portProblem ?? undefined}
                help="Optional. Fixes the port; the relay otherwise picks a free one and keeps it."
              >
                <Input
                  id="nt-port"
                  type="number"
                  min={1}
                  max={65535}
                  value={port}
                  invalid={portErr !== null || portProblem !== null}
                  aria-describedby="nt-port-desc"
                  onChange={(e) => { setPort(e.target.value); setPortErr(null); }}
                />
              </FormField>
            )}
            <FormField
              label="Port of the local service"
              htmlFor="nt-local"
              w="sm"
              descId="nt-local-desc"
              help="Only for the command shown afterwards, such as 3389 for Remote Desktop."
            >
              <Input
                id="nt-local"
                type="number"
                min={1}
                max={65535}
                value={localPort}
                aria-describedby="nt-local-desc"
                onChange={(e) => setLocalPort(e.target.value)}
              />
            </FormField>
          </FormFieldGroup>
          {err && <ErrorNotice>{err}</ErrorNotice>}
        </>
      )}
    </Dialog>
  );
}
