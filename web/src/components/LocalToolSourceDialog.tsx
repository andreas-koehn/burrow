import { useEffect, useId, useRef, useState } from "react";
import type { RefObject } from "react";
import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, SubmitHint, firstMissing } from "@/components/ds";
import { CopyableCode } from "@/components/CopyableCode";
import { InstallLines } from "@/components/InstallLines";
import { StatusBadge } from "@/components/StatusBadge";
import { serviceState } from "@/lib/status";
import { burrowMcpCommand, burrowYamlSnippet, toolSlugError, toolSlugFromName } from "@/lib/toolSource";
import { useKnownClients } from "@/lib/useKnownClients";
import type { Service, ToolSource, ToolSyncResult } from "@/lib/contract";

export interface LocalToolSourceDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/**
 * Puts a tool server that runs on one of the reader's machines behind the
 * gateway, in two steps: name it, then run one command on the machine. The
 * first step creates a service that only the gateway reaches and the tool
 * source on top of it; the second waits until a client serves it, then reads
 * its tools once.
 */
export function LocalToolSourceDialog({ open, onOpenChange }: LocalToolSourceDialogProps) {
  // Mounted per opening, so every opening starts from an empty form.
  return open ? <Flow onOpenChange={onOpenChange} /> : null;
}

const STILL_WAITING_MS = 60_000;
const INSTALL_ONLY = ["install", "login"] as const;

function Flow({ onOpenChange }: Pick<LocalToolSourceDialogProps, "onOpenChange">) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const reasonId = useId();
  const canListClients = useCan(P.tunnelsReadAny);
  const known = useKnownClients({ enabled: canListClients });
  const [name, setName] = useState("");
  const [nameLeft, setNameLeft] = useState(false);
  const [program, setProgram] = useState("");
  const [nameErr, setNameErr] = useState<ReactNode>(null);
  const [formErr, setFormErr] = useState<string | null>(null);
  // What exists already: the service after the first request, the source after the second.
  const [service, setService] = useState<string | null>(null);
  const [source, setSource] = useState<ToolSource | null>(null);
  const [newMachine, setNewMachine] = useState(false);

  const slug = toolSlugFromName(name);
  const slugProblem = toolSlugError(slug);
  // The service is named as the command will name it, and a service id has three characters at least.
  const nameProblem = name.trim() === "" ? "Enter a name."
    : slugProblem !== null ? slugProblem
    : slug.length < 3 ? "Use a name with at least three letters or digits." : null;
  const programProblem = program.trim() === "" ? "Enter the command." : null;
  const missing = firstMissing([nameProblem && "Fix the name.", programProblem]);

  const create = useMutation({
    mutationFn: async () => {
      let made = service;
      if (made === null) {
        // Nothing is created next to a source that is there already.
        const sources = await qc.fetchQuery({ queryKey: ["mcp", "sources"], queryFn: () => apiFetch<ToolSource[]>("/mcp/sources"), staleTime: 0 })
          .catch(() => [] as ToolSource[]);
        if (sources.some((s) => s.slug === slug)) {
          setNameErr(`A tool source ${slug} already exists. Choose another name.`);
          return null;
        }
        try {
          // A client finds the service it serves by name, so the service is named as the command will name it.
          await apiFetch<{ id: string }>("/services", {
            method: "POST",
            body: JSON.stringify({ service_id: slug, title: slug, access_mode: "api_key" }),
          });
        } catch (e) {
          if (!(e instanceof ApiError)) setFormErr("Couldn't create the service.");
          else if (e.status === 403) setFormErr("Creating the service needs the permission to configure services.");
          else if (e.status === 409) setNameErr(`A service named ${slug} already exists. Choose another name.`);
          else setFormErr(e.message);
          return null;
        }
        made = slug;
        setService(made);
        void qc.invalidateQueries({ queryKey: ["services"] });
      }
      try {
        return await apiFetch<ToolSource>("/mcp/sources", {
          method: "POST",
          body: JSON.stringify({ slug, name: name.trim(), kind: "tunnel", service_id: made }),
        });
      } catch (e) {
        if (e instanceof ApiError && e.status === 403) setFormErr("Adding a tool source needs the permission to manage tool sources.");
        else {
          const why = e instanceof ApiError ? e.message : "the request failed";
          setFormErr(`Service ${made} was created, but the tool source was not: ${why}. Retry creates the tool source only.`);
        }
        return null;
      }
    },
    onSuccess: (made) => {
      if (!made) return;
      void qc.invalidateQueries({ queryKey: ["mcp", "sources"] });
      setSource(made);
    },
  });

  const runHead = useRef<HTMLHeadingElement>(null);
  const created = source !== null;
  useEffect(() => {
    if (created) runHead.current?.focus();
  }, [created]);

  if (source !== null) {
    const hasClient = (known.rows?.length ?? 0) > 0;
    const showInstall = newMachine || !hasClient;
    const origin = typeof window !== "undefined" ? window.location.origin : "";
    return (
      <RunStep
        source={source}
        program={program}
        showInstall={showInstall}
        hasClient={hasClient}
        newMachine={newMachine}
        onNewMachine={() => setNewMachine((on) => !on)}
        origin={origin}
        headRef={runHead}
        onDone={(to) => { onOpenChange(false); nav(to); }}
      />
    );
  }

  const nameMessage = nameErr ?? (nameLeft ? nameProblem : null);
  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!create.isPending) onOpenChange(o); }}
      title="Add local source"
      description="Put a tool server that runs on one of your machines behind the tool gateway."
      footer={
        <>
          <SubmitHint id={reasonId} missing={[missing]} />
          <Button variant="secondary" disabled={create.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={missing !== undefined || create.isPending}
            aria-describedby={missing ? reasonId : undefined}
            onClick={() => { setFormErr(null); create.mutate(); }}
          >
            {create.isPending ? "Creating…" : service ? "Retry" : "Continue"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField
          label="Name"
          htmlFor="ls-name"
          w="md"
          descId="ls-name-desc"
          error={nameMessage ?? undefined}
          help={nameProblem === null ? <>Served at <span className="mono">/mcp/{slug}</span>.</> : "Its address is made from the name."}
        >
          <Input
            id="ls-name"
            value={name}
            maxLength={120}
            required
            // The service is named after it: once it exists the name stays.
            disabled={service !== null}
            autoComplete="off"
            invalid={nameMessage !== null}
            aria-describedby="ls-name-desc"
            onChange={(e) => { setName(e.target.value); setNameErr(null); setFormErr(null); }}
            onBlur={() => setNameLeft(true)}
          />
        </FormField>
        <FormField
          label="Command or address"
          htmlFor="ls-program"
          descId="ls-program-desc"
          help={<>The command that starts the tool server, for example <span className="mono">npx -y @modelcontextprotocol/server-filesystem /srv</span>, or the address of one that already listens, <span className="mono">http://localhost:3001/mcp</span>.</>}
        >
          <Input
            id="ls-program"
            mono
            value={program}
            required
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-describedby="ls-program-desc"
            onChange={(e) => { setProgram(e.target.value); setFormErr(null); }}
          />
        </FormField>
      </FormFieldGroup>
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}

interface RunStepProps {
  source: ToolSource;
  program: string;
  showInstall: boolean;
  hasClient: boolean;
  newMachine: boolean;
  onNewMachine: () => void;
  origin: string;
  headRef: RefObject<HTMLHeadingElement | null>;
  onDone: (to: string) => void;
}

function RunStep({ source, program, showInstall, hasClient, onNewMachine, newMachine, origin, headRef, onDone }: RunStepProps) {
  const qc = useQueryClient();
  const slug = source.slug;
  const [result, setResult] = useState<ToolSyncResult | null>(null);
  const [syncErr, setSyncErr] = useState<string | null>(null);

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
    gcTime: 0,
    refetchInterval: (q) => {
      const svc = q.state.data?.find((s) => s.id === source.service_id);
      return svc && serviceState(svc) === "online" ? false : 2000;
    },
  });
  const svc = services.data?.find((s) => s.id === source.service_id);
  const online = svc !== undefined && serviceState(svc) === "online";

  const sync = useMutation({
    mutationFn: () => apiFetch<ToolSyncResult>(`/mcp/sources/${encodeURIComponent(slug)}/sync`, { method: "POST" }),
    onSuccess: (r) => {
      setSyncErr(null);
      setResult(r);
      void qc.invalidateQueries({ queryKey: ["mcp", "sources"] });
    },
    onError: (e: unknown) => setSyncErr(e instanceof ApiError ? e.message : "The tools could not be read."),
  });
  // The first read happens by itself once a client serves the source; a failed one is asked for again by the reader.
  const asked = useRef(false);
  useEffect(() => {
    if (!online || asked.current) return;
    asked.current = true;
    sync.mutate();
  }, [online, sync]);

  const command = burrowMcpCommand(slug, program);
  // A minute without a client: say what to check, and keep asking.
  const [waitedLong, setWaitedLong] = useState(false);
  useEffect(() => {
    if (online) return;
    const t = setTimeout(() => setWaitedLong(true), STILL_WAITING_MS);
    return () => clearTimeout(t);
  }, [online]);

  const noun = (n: number) => (n === 1 ? "1 tool" : `${n} tools`);
  const said = result !== null ? (
    <>
      <StatusBadge state="online" />
      <span>
        {result.tool_count === 0 ? "The server offers no tools."
          : result.pending_count > 0 ? `${noun(result.tool_count)} found, ${result.pending_count} to approve`
          : `${noun(result.tool_count)} found`}
      </span>
    </>
  ) : syncErr !== null ? (
    <>
      <StatusBadge state="online" />
      <span>{syncErr}</span>
      <Button variant="secondary" size="sm" disabled={sync.isPending} onClick={() => sync.mutate()}>Read the tools again</Button>
    </>
  ) : online ? (
    <>
      <StatusBadge state="online" />
      <span className="muted">Reading the tools…</span>
    </>
  ) : (
    <>
      <span className="spinner" aria-hidden="true" />
      <span className="muted">Waiting for the client…</span>
      {waitedLong && <span className="muted small">Still waiting? Check that the burrow mcp command is running and signed in.</span>}
    </>
  );

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o) onDone("/gateway/tools"); }}
      title="Add local source"
      description={`${source.name} is created. It is served once a client runs it.`}
      footer={
        <Button variant="primary" onClick={() => onDone(`/gateway/tools/${slug}`)}>
          {result !== null && result.tool_count > 0 ? "Review the tools" : "Done"}
        </Button>
      }
    >
      <h4 ref={headRef} tabIndex={-1} className="section-label">Run this on the machine</h4>
      {showInstall && <InstallLines relayOrigin={origin} lines={INSTALL_ONLY} />}
      {hasClient && (
        <div>
          <Button variant="ghost" size="sm" aria-expanded={newMachine} onClick={onNewMachine}>This is a new machine</Button>
        </div>
      )}
      <CopyableCode text={command} label="Copy run command" />
      <p className="muted small">
        Add <code>--env KEY=VALUE</code> for each variable the server needs. The client restarts the server when it stops.
      </p>
      <h4 className="section-label">Or keep it in burrow.yaml</h4>
      <CopyableCode text={burrowYamlSnippet(slug, program)} label="Copy burrow.yaml service" />
      <p className="muted small">Then run <code>burrow up</code>.</p>
      <div className="connect-wait">
        <div role="status" className="row row-center gap-2">{said}</div>
      </div>
    </Dialog>
  );
}
