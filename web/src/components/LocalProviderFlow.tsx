import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { Button, ErrorNotice, FormField, FormFieldGroup, Input, Select, SubmitHint } from "@/components/ds";
import { InstallLines } from "@/components/InstallLines";
import { WaitForProvider } from "@/components/WaitForProvider";
import { isTarget } from "@/lib/installSnippets";
import { providerSlugError } from "@/lib/providerSlug";
import { providerBaseUrl } from "@/lib/serviceUrl";
import { useKnownClients } from "@/lib/useKnownClients";
import type { AiProvider, Service } from "@/lib/contract";

export interface LocalProviderFlowProps {
  /** The dialog is closed: by Cancel, by Done, or any other way. */
  onDone: () => void;
  /** The reader has a service already and wants to choose it instead. */
  onUseExisting: () => void;
}

/** What the flow shows right now, for the dialog that holds it. */
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
  const canListClients = useCan(P.tunnelsReadAny);
  // Asked for from the first step on, so the second knows at once which lines to show.
  const known = useKnownClients({ enabled: canListClients });

  const [name, setName] = useState("");
  const [nameLeft, setNameLeft] = useState(false);
  const [server, setServer] = useState(SERVERS[0]!.value);
  const [address, setAddress] = useState("");
  const [addressLeft, setAddressLeft] = useState(false);
  const [format, setFormat] = useState<Format>("openai");
  // nameErr is the relay's verdict on the name; formErr is everything else.
  const [nameErr, setNameErr] = useState<ReactNode>(null);
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

  /**
   * The provider that goes by this slug already, or that the service of this
   * name already has: the one these steps made before, when the dialog was
   * closed while it waited. Asked for when the steps are sent off, from the
   * lists the pages behind the dialog show.
   */
  async function providerFor(wanted: string): Promise<AiProvider | undefined> {
    try {
      const [providers, services] = await Promise.all([
        qc.fetchQuery({ queryKey: ["ai", "providers"], queryFn: () => apiFetch<AiProvider[]>("/ai/providers") }),
        qc.fetchQuery({ queryKey: ["services"], queryFn: () => apiFetch<Service[]>("/services") }),
      ]);
      const backing = (Array.isArray(services) ? services : []).find((s) => s.id === wanted || s.name === wanted);
      return (Array.isArray(providers) ? providers : [])
        .find((p) => p.slug === wanted || (backing !== undefined && p.service_id === backing.id));
    } catch {
      // The requests that follow say what is wrong.
      return undefined;
    }
  }

  const create = useMutation({
    mutationFn: async (v: { slug: string; name: string; format: Format }) => {
      let made = service;
      if (made === null) {
        // Nothing is created next to a provider that is there already.
        const taken = await providerFor(v.slug);
        if (taken) {
          const what = taken.slug === v.slug
            ? `There already is a provider ${taken.slug}.`
            : `The service ${v.slug} already has a provider, ${taken.slug}.`;
          setNameErr(taken.kind === "tunnel" ? (
            <>
              {what}{" "}
              {/* Where its command is, and where the wait for its client goes on. */}
              <Link className="link-inline" to={`/clients/connect?provider=${encodeURIComponent(taken.slug)}`} onClick={onDone}>
                Bring it online
              </Link>, or choose another name.
            </>
          ) : `${what} Choose another name.`);
          return null;
        }
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
          else if (e.status === 403) setFormErr("Adding a provider needs the permission to manage providers.");
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
        if (e instanceof ApiError && e.status === 403) setFormErr("Adding a provider needs the permission to manage providers.");
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
        <SubmitHint id={reasonId} missing={[missing]} />
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
