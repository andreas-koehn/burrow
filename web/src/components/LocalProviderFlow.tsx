import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, Select } from "@/components/ds";
import { InstallLines } from "@/components/InstallLines";
import { StatusBadge } from "@/components/StatusBadge";
import { providerSlugError } from "@/lib/providerSlug";
import { providerBaseUrl } from "@/lib/serviceUrl";
import { providerState } from "@/lib/status";
import { KNOWN_CLIENTS_KEY, useKnownClients } from "@/lib/useKnownClients";
import type { AiProvider } from "@/lib/contract";

export interface LocalProviderFlowProps {
  /** The dialog is closed: by Cancel, by Done, or any other way. */
  onDone: () => void;
  /** The reader has a service already and wants to choose it instead. */
  onUseExisting: () => void;
}

/** What the flow shows right now, for a dialog that is not the flow's own. */
export interface LocalProviderFlowParts {
  step: "name" | "run";
  description: string;
  body: ReactNode;
  footer: ReactNode;
}

type Format = "openai" | "anthropic";

const SERVERS = [
  { value: "ollama", label: "Ollama · 11434", target: "11434" },
  { value: "lmstudio", label: "LM Studio · 1234", target: "1234" },
  { value: "other", label: "Other port or address", target: "" },
];
const SERVER_OPTIONS = SERVERS.map(({ value, label }) => ({ value, label }));
const FORMAT_OPTIONS = [
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic" },
];
const RUN_ONLY = ["run"] as const;

/**
 * The relay's own rule for the slug of a provider that is given none: lower
 * case, every run of other characters one hyphen, none at either end, 40
 * characters at most.
 */
function slugFromName(name: string): string {
  let slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (slug.length > 40) slug = slug.slice(0, 40).replace(/-+$/, "");
  return slug;
}

/** What `burrow http` publishes: a port, or host:port. */
function isTarget(value: string): boolean {
  const m = /^(?:(?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:.]+\]):)?(\d{1,5})$/.exec(value);
  if (!m) return false;
  const port = Number(m[1]);
  return port >= 1 && port <= 65535;
}

// How often a provider that is online is asked again for a catalog it does not have yet.
const CATALOG_ANSWERS = 5;

export interface WaitForProviderProps {
  slug: string;
  /** Shown below the line once the provider is online: what to do next. */
  children?: ReactNode;
}

/**
 * The line a reader waits on after the command: it asks for the provider
 * every two seconds until a client serves it, then says Online and how many
 * models the catalog has. An answer that fails ends the asking.
 */
export function WaitForProvider({ slug, children }: WaitForProviderProps) {
  const qc = useQueryClient();
  // The number of the answer that first said online.
  const onlineSince = useRef<number | null>(null);
  const provider = useQuery({
    // Not below ["ai", "providers"]: refreshing the list must not ask here again.
    queryKey: ["ai", "provider-status", slug],
    queryFn: () => apiFetch<AiProvider>(`/ai/providers/${encodeURIComponent(slug)}`),
    retry: false,
    // An answer from an earlier wait says nothing about now.
    gcTime: 0,
    refetchInterval: (query) => {
      if (query.state.status === "error") return false;
      const p = query.state.data;
      if (!p || providerState(p) !== "online") {
        onlineSince.current = null;
        return 2000;
      }
      if (p.model_count > 0) return false;
      // The catalog is read after the client has connected: ask a little longer for it.
      onlineSince.current ??= query.state.dataUpdateCount;
      return query.state.dataUpdateCount - onlineSince.current < CATALOG_ANSWERS ? 2000 : false;
    },
  });
  const p = provider.data;
  const online = p !== undefined && providerState(p) === "online";

  // The lists behind this line are not asked every two seconds: tell them once.
  useEffect(() => {
    if (!online) return;
    void qc.invalidateQueries({ queryKey: KNOWN_CLIENTS_KEY });
    void qc.invalidateQueries({ queryKey: ["ai", "providers"] });
  }, [online, qc]);

  return (
    <>
      <div role="status" className="connect-wait">
        {online ? (
          <>
            <StatusBadge state="online" />
            {p.model_count > 0 && <span>{p.model_count === 1 ? "1 model found" : `${p.model_count} models found`}</span>}
          </>
        ) : provider.isError ? (
          <span className="muted">The status could not be read. The provider's page shows it.</span>
        ) : (
          <>
            <span className="spinner" aria-hidden="true" />
            <span className="muted">Waiting for the client…</span>
          </>
        )}
      </div>
      {online && children}
    </>
  );
}

/**
 * Puts a model server that runs on one of the reader's machines behind the
 * gateway, in two steps: name it, then run one command on the machine. The
 * first step creates a service that only the gateway reaches and the provider
 * on top of it; the second waits until a client serves it.
 *
 * Returned as parts, so that "New provider" can show them in the dialog it
 * already has open: a second dialog would take the focus away from the choice
 * that was just made.
 */
export function useLocalProviderFlow({ onDone, onUseExisting }: LocalProviderFlowProps): LocalProviderFlowParts {
  const qc = useQueryClient();
  const nav = useNavigate();
  const reasonId = useId();
  const isAdmin = useAuth().user?.role === "admin";
  // Asked for from the first step on, so the second knows at once which lines to show.
  const known = useKnownClients({ enabled: isAdmin });

  const [name, setName] = useState("");
  const [nameLeft, setNameLeft] = useState(false);
  const [server, setServer] = useState(SERVERS[0]!.value);
  const [address, setAddress] = useState("");
  const [addressLeft, setAddressLeft] = useState(false);
  const [format, setFormat] = useState<Format>("openai");
  // nameErr is the relay's verdict on the name; formErr is everything else.
  const [nameErr, setNameErr] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);
  // What exists already: the service after the first request, the provider after the second.
  const [service, setService] = useState<{ id: string; name: string } | null>(null);
  const [provider, setProvider] = useState<AiProvider | null>(null);
  const [newMachine, setNewMachine] = useState(false);

  const slug = slugFromName(name);
  const preset = SERVERS.find((s) => s.value === server)!;
  const target = preset.target || address.trim();
  const nameProblem = name.trim() === ""
    ? "Enter a name."
    : slug.length < 3 || providerSlugError(slug) !== null ? "Use a name with at least three letters or digits." : null;
  const addressProblem = isTarget(target) ? null : "Enter host:port or a port.";
  const missing = nameProblem ?? addressProblem;

  const create = useMutation({
    mutationFn: async (v: { slug: string; name: string; format: Format }) => {
      let made = service;
      if (made === null) {
        try {
          // A client finds the service it serves by name, so the service is
          // named as the command will name it. Only the gateway reaches it.
          const res = await apiFetch<{ id: string }>("/services", {
            method: "POST",
            body: JSON.stringify({ service_id: v.slug, title: v.slug, access_mode: "api_key" }),
          });
          made = { id: res?.id || v.slug, name: v.slug };
        } catch (e) {
          if (!(e instanceof ApiError)) setFormErr("Couldn't create the service.");
          else if (e.status === 403) setFormErr("Adding a provider needs an admin.");
          else if (e.status === 409) setNameErr(`A service named ${v.slug} already exists. Choose another name, or use that service.`);
          else setFormErr(e.message);
          return null;
        }
        setService(made);
        void qc.invalidateQueries({ queryKey: ["services"] });
      }
      try {
        // The service only becomes reachable under /ai/ once it has a provider.
        return await apiFetch<AiProvider>("/ai/providers", {
          method: "POST",
          body: JSON.stringify({ name: v.name, kind: "tunnel", service_id: made.id, gateway_only: true, api_format: v.format }),
        });
      } catch (e) {
        if (e instanceof ApiError && e.status === 403) setFormErr("Adding a provider needs an admin.");
        else {
          const why = e instanceof ApiError ? e.message : "the request failed";
          setFormErr(`Service ${made.id} was created, but the provider was not: ${why}. Retry creates the provider only.`);
        }
        return null;
      }
    },
    onSuccess: (created) => {
      if (!created) return;
      void qc.invalidateQueries({ queryKey: ["ai", "providers"] });
      setProvider(created);
    },
  });

  // The button that was pressed is gone with its step: the next one takes the focus.
  const runHead = useRef<HTMLHeadingElement>(null);
  const created = provider !== null;
  useEffect(() => {
    if (created) runHead.current?.focus();
  }, [created]);

  if (provider !== null && service !== null) {
    const hasClient = (known.rows?.length ?? 0) > 0;
    const allLines = newMachine || !hasClient;
    const origin = typeof window !== "undefined" ? window.location.origin : "";
    const query = `new=1&provider=${encodeURIComponent(provider.slug)}`;
    return {
      step: "run",
      description: `${provider.name} is created. It comes online once a client serves it.`,
      body: (
        <>
          <h4 ref={runHead} tabIndex={-1} className="section-label">Run this on the machine</h4>
          <InstallLines relayOrigin={origin} target={target} name={service.name} lines={allLines ? undefined : RUN_ONLY} />
          {hasClient && (
            <div>
              <Button variant="ghost" size="sm" aria-expanded={newMachine} onClick={() => setNewMachine((on) => !on)}>
                This is a new machine
              </Button>
            </div>
          )}
          <WaitForProvider slug={provider.slug}>
            {/* Each leads away from this page: the dialog is closed first, so the way back does not open it again. */}
            <div className="row row-center gap-2">
              <Link className="btn btn-secondary btn-sm" to={`/gateway/models?${query}`} onClick={onDone}>Create a model</Link>
              <Link className="btn btn-secondary btn-sm" to={`/gateway/keys?${query}`} onClick={onDone}>Create a gateway key</Link>
            </div>
          </WaitForProvider>
        </>
      ),
      footer: (
        <Button variant="primary" onClick={() => { onDone(); nav(`/gateway/providers/${provider.slug}`); }}>Done</Button>
      ),
    };
  }

  const nameMessage = nameErr ?? (nameLeft ? nameProblem : null);
  const addressMessage = addressLeft && addressProblem ? "Enter host:port or a port, for example 127.0.0.1:8000 or 8000." : null;
  const preview = nameProblem === null ? providerBaseUrl(slug) : "";
  return {
    step: "name",
    description: "Put a model server that runs on one of your machines behind the AI gateway.",
    body: (
      <>
        <FormFieldGroup>
          <FormField
            label="Name"
            htmlFor="lp-name"
            w="md"
            descId="lp-name-desc"
            error={nameMessage ?? undefined}
            help={preview ? <span className="mono">{preview}</span> : "Its address is made from the name."}
          >
            <Input
              id="lp-name"
              value={name}
              maxLength={120}
              required
              invalid={nameMessage !== null}
              autoComplete="off"
              aria-describedby="lp-name-desc"
              onChange={(e) => { setName(e.target.value); setNameErr(null); setFormErr(null); }}
              onBlur={() => setNameLeft(true)}
            />
          </FormField>
          <FormField label="Model server" htmlFor="lp-server" w="md">
            <Select id="lp-server" value={server} onChange={(v) => { setServer(v); setFormErr(null); }} options={SERVER_OPTIONS} />
          </FormField>
          {preset.target === "" && (
            <FormField
              label="Port or address"
              htmlFor="lp-address"
              w="md"
              descId="lp-address-desc"
              error={addressMessage ?? undefined}
              help="Where the model server listens, as the machine reaches it: a port, or host:port."
            >
              <Input
                id="lp-address"
                mono
                value={address}
                maxLength={255}
                required
                invalid={addressMessage !== null}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                aria-describedby="lp-address-desc"
                onChange={(e) => { setAddress(e.target.value); setFormErr(null); }}
                onBlur={() => setAddressLeft(true)}
              />
            </FormField>
          )}
          <FormField label="API format" htmlFor="lp-format" w="md" descId="lp-format-desc" help="The API the model server speaks.">
            <Select
              id="lp-format"
              value={format}
              onChange={(v) => { setFormat(v as Format); setFormErr(null); }}
              options={FORMAT_OPTIONS}
              aria-describedby="lp-format-desc"
            />
          </FormField>
        </FormFieldGroup>
        <div>
          <Button variant="ghost" size="sm" onClick={onUseExisting}>Use a service that already exists</Button>
        </div>
        {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
      </>
    ),
    footer: (
      <>
        {missing && <span id={reasonId} className="muted small" style={{ alignSelf: "center" }}>{missing}</span>}
        <Button variant="secondary" onClick={onDone}>Cancel</Button>
        <Button
          variant="primary"
          disabled={missing !== null || create.isPending}
          aria-describedby={missing ? reasonId : undefined}
          onClick={() => { setFormErr(null); create.mutate({ slug, name: name.trim(), format }); }}
        >
          {create.isPending ? "Creating…" : service ? "Retry" : "Continue"}
        </Button>
      </>
    ),
  };
}

/** The guided steps in a dialog of their own. */
export function LocalProviderFlow(props: LocalProviderFlowProps) {
  const flow = useLocalProviderFlow(props);
  const { onDone } = props;
  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onDone(); }}
      title="New provider"
      description={flow.description}
      footer={flow.footer}
    >
      {flow.body}
    </Dialog>
  );
}
