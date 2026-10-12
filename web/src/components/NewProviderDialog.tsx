import { useEffect, useId, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Combobox, Dialog, Disclosure, ErrorNotice, FormField, FormFieldGroup, Input, Select, SubmitHint, firstMissing } from "@/components/ds";
import { SlugField } from "@/components/SlugField";
import { useLocalProviderFlow } from "@/components/LocalProviderFlow";
import { ConcurrencyField, ResponsesCheckbox, SlotStatus } from "@/components/ProviderUpstreamPanel";
import { providerSlugError } from "@/lib/providerSlug";
import {
  CREDENTIAL_SLOT_HELP, NO_CREDENTIAL_SLOTS, PROVIDER_PRESETS, credentialSlotsError, parseConcurrency, slotSuggestions, splitSlots,
} from "@/lib/providerPresets";
import { providerBaseUrl } from "@/lib/serviceUrl";
import type { AiProvider, Service } from "@/lib/contract";

export interface NewProviderDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/**
 * Adds a model provider: a model server on one of the reader's machines, in
 * guided steps or from an http service in API-key mode that exists already, or
 * a hosted API the relay calls itself (kind "direct"). A hosted API's
 * credential is set on the relay; this dialog only names its slot.
 */
export function NewProviderDialog({ open, onOpenChange }: NewProviderDialogProps) {
  // Mounted per opening, so every opening starts from an empty form.
  return open ? <NewProviderForm onOpenChange={onOpenChange} /> : null;
}

type Kind = "local" | "direct";

const BILLING_OPTIONS = [
  { value: "metered", label: "Metered" },
  { value: "flat", label: "Flat rate" },
];
const PRESET_OPTIONS = PROVIDER_PRESETS.map((p) => ({ value: p.id, label: p.label }));
type ApiFormat = AiProvider["api_format"];
const FORMAT_OPTIONS = [
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic" },
];

function NewProviderForm({ onOpenChange }: Pick<NewProviderDialogProps, "onOpenChange">) {
  const qc = useQueryClient();
  const kindId = useId();
  const [kind, setKind] = useState<Kind>("local");
  // A model server is set up in guided steps, unless its service is there already.
  const [existing, setExisting] = useState(false);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [serviceId, setServiceId] = useState("");
  // Hosted API only.
  const [presetId, setPresetId] = useState("");
  const [apiFormat, setApiFormat] = useState<ApiFormat>("openai");
  const [baseUrl, setBaseUrl] = useState("");
  const [slot, setSlot] = useState("");
  const [billing, setBilling] = useState<"metered" | "flat">("metered");
  // The operator's statement that the API offers POST /responses.
  const [responses, setResponses] = useState(false);
  // Requests served at once; empty = no limit.
  const [concurrency, setConcurrency] = useState("");
  const [concurrencyErr, setConcurrencyErr] = useState<string | null>(null);
  // slugErr, urlErr and slotErr belong to their fields; formErr is everything
  // else (name, service taken or not eligible, permission, network).
  const [slugErr, setSlugErr] = useState<string | null>(null);
  const [urlErr, setUrlErr] = useState<string | null>(null);
  const [slotErr, setSlotErr] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);
  // Hosted API only: slug, base URL, requests at once and the Responses API.
  const [advanced, setAdvanced] = useState(false);
  // The slot's rule is named once its field was left, not while it is typed in.
  const [slotLeft, setSlotLeft] = useState(false);
  const direct = kind === "direct";
  const guided = kind === "local" && !existing;

  // The button that leads from one form to the other goes away with its form:
  // the focus moves on to the name field of the form that takes its place.
  const moved = useRef(false);
  function showExisting(on: boolean) {
    moved.current = true;
    setExisting(on);
    clearErrors();
  }
  useEffect(() => {
    if (!moved.current) return;
    moved.current = false;
    document.getElementById(existing ? "np-name" : "lp-name")?.focus();
  }, [existing]);
  // Called in every state, so what was typed into the guided steps is still
  // there after a look at the other choice.
  const flow = useLocalProviderFlow({
    onDone: () => onOpenChange(false),
    onUseExisting: () => showExisting(true),
  });

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  // Names of the credential slots set on the relay; never their values.
  const slots = useQuery({
    queryKey: ["upstream-credential-slots"],
    queryFn: () => apiFetch<{ slots: string[] }>("/upstream-credentials/slots"),
    retry: false,
    enabled: direct,
  });

  const taken = new Set((providers.data ?? []).map((p) => p.service_id));
  const eligible = (services.data ?? []).filter(
    (s) => s.type === "http" && s.access_mode === "api_key" && !taken.has(s.id),
  );
  const loaded = services.data !== undefined && providers.data !== undefined;
  const loadFailed = services.isError || providers.isError;
  // With a single candidate there is nothing to choose.
  const chosen = serviceId || (eligible.length === 1 ? eligible[0]!.id : "");

  const preset = PROVIDER_PRESETS.find((p) => p.id === presetId);
  function choosePreset(id: string) {
    const p = PROVIDER_PRESETS.find((x) => x.id === id);
    if (!p) return;
    // Choosing a preset fills every field below it, also on a second choice.
    setPresetId(p.id);
    setName(p.name);
    setSlug(p.slug);
    setApiFormat(p.apiFormat);
    setBaseUrl(p.baseUrl);
    setSlot(p.credentialSlot);
    setBilling(p.billing);
    setResponses(p.supportsResponses);
    // A preset without a base URL ("Other API") has one to ask for.
    if (p.baseUrl === "") setAdvanced(true);
    setSlotLeft(false);
    clearErrors();
  }
  function clearErrors() {
    setSlugErr(null); setUrlErr(null); setSlotErr(null); setConcurrencyErr(null); setFormErr(null);
  }

  const limit = parseConcurrency(concurrency);
  // The Responses API belongs to the OpenAI format: another format never claims it.
  const offersResponses = apiFormat === "openai" && responses;
  const create = useMutation({
    mutationFn: () =>
      apiFetch<AiProvider>("/ai/providers", {
        method: "POST",
        body: JSON.stringify(direct
          ? { kind: "direct", slug: slug || undefined, name: name.trim(), api_format: apiFormat, base_url: baseUrl.trim(), credential_slot: splitSlots(slot).join(","), billing, supports_responses: offersResponses, max_concurrent: limit ?? 0 }
          : { slug: slug || undefined, name: name.trim(), kind: "tunnel", service_id: chosen }),
      }),
    // Wait for the refetch so the list behind the dialog already shows the provider.
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["ai", "providers"] });
      onOpenChange(false);
    },
    onError: (e: unknown) => {
      // A field under Advanced is shown with what the relay said about it,
      // and stays shown while it is corrected.
      const underAdvanced = (set: (message: string) => void, message: string) => { set(message); if (direct) setAdvanced(true); };
      if (!(e instanceof ApiError)) setFormErr("Couldn't create the provider.");
      // Only the slug rule ("slug must be …") is about the slug.
      else if (e.status === 400 && e.message.startsWith("slug")) underAdvanced(setSlugErr, e.message);
      // A hosted API has no service to be taken: its only conflict is the slug.
      else if (direct && e.status === 409) underAdvanced(setSlugErr, e.message);
      else if (direct && e.status === 400 && e.message.startsWith("base URL")) underAdvanced(setUrlErr, e.message);
      else if (direct && e.status === 400 && e.message.startsWith("credential slot")) setSlotErr(e.message);
      else if (direct && e.status === 400 && e.message.startsWith("max concurrent")) underAdvanced(setConcurrencyErr, e.message);
      else if (e.status === 403) setFormErr("You don't have permission to add providers.");
      else setFormErr(e.message);
    },
  });

  const slugProblem = providerSlugError(slug);
  const slotProblem = credentialSlotsError(slot);
  const slotMessage = slotErr ?? (slotLeft ? slotProblem : null);
  // The list names every slot that is set, also one set to an empty value, so
  // a listed slot is no proof of a usable key; an unlisted one is proof of none.
  const known = slots.data?.slots;
  // Beside Create while it is disabled, the first of these that applies; top to bottom as the form reads.
  const missing = direct ? [
    presetId === "" && "Choose a provider.",
    name.trim() === "" && "Enter a name.",
    splitSlots(slot).length === 0 && "Enter a credential slot.",
    slotProblem !== null && "Check the credential slot.",
    baseUrl.trim() === "" && "Enter a base URL.",
    slugProblem !== null && "Check the provider slug.",
    limit === null && "Check the requests at once.",
  ] : [
    name.trim() === "" && "Enter a name.",
    slugProblem !== null && "Check the provider slug.",
    chosen === "" && (loaded && eligible.length === 0 ? "There is no service to use." : "Choose a service."),
  ];
  const hint = firstMissing(missing);
  // A field that holds the button back is never left hidden.
  const advancedOpen = advanced || slugProblem !== null || limit === null || (presetId !== "" && baseUrl.trim() === "");

  // The relay refused one field: it takes the focus. Where the field is under
  // Advanced, the same answer has opened it.
  useEffect(() => {
    const field = slugErr !== null ? "np-slug" : urlErr !== null ? "np-base-url" : concurrencyErr !== null ? "np-concurrency"
      : slotErr !== null ? "np-slot" : null;
    if (field) document.getElementById(field)?.focus();
  }, [slugErr, urlErr, concurrencyErr, slotErr]);

  const slugField = (
    <SlugField
      id="np-slug"
      label="Provider slug"
      value={slug}
      onChange={(v) => { setSlug(v); setSlugErr(null); setFormErr(null); }}
      error={slugErr}
      rule={providerSlugError}
      errorAfterBlur
      help="Leave the slug empty to derive it from the name."
      preview={(s) => providerBaseUrl(s)}
    />
  );
  return (
    <Dialog
      open
      onOpenChange={onOpenChange}
      title="New provider"
      description={guided
        ? flow.description
        : direct
          ? "Serve a hosted API under its own base URL. The relay calls it with a credential set on the relay."
          : "Serve a service in API-key mode as a model provider under its own base URL."}
      footer={guided ? flow.footer : (
        <>
          <SubmitHint id="np-hint" missing={missing} />
          <Button variant="secondary" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={hint !== undefined || create.isPending}
            aria-describedby={hint !== undefined ? "np-hint" : undefined}
            onClick={() => { clearErrors(); create.mutate(); }}
          >
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </>
      )}
    >
      {/* Once the provider exists there is nothing left to choose. */}
      {!(guided && flow.step === "run") && (
        <div role="radiogroup" aria-labelledby={kindId} className="col gap-2">
          <span id={kindId} className="muted small">What the provider serves</span>
          {([
            ["local", "A model server on one of my machines"],
            ["direct", "A hosted API"],
          ] as const).map(([value, label]) => (
            <label key={value} className="row row-center gap-2">
              <input
                type="radio"
                name={kindId}
                value={value}
                checked={kind === value}
                onChange={() => { setKind(value); clearErrors(); }}
              />
              <span>{label}</span>
            </label>
          ))}
        </div>
      )}
      {guided ? flow.body : (
        <>
          <FormFieldGroup>
            {direct && (
              <FormField label="Provider" htmlFor="np-preset" w="md" help={preset?.note}>
                <Select
                  id="np-preset"
                  value={presetId}
                  onChange={choosePreset}
                  options={PRESET_OPTIONS}
                  placeholder="Choose a provider…"
                />
              </FormField>
            )}
            <FormField label="Name" htmlFor="np-name" w="md">
              <Input
                id="np-name"
                value={name}
                maxLength={120}
                required
                autoComplete="off"
                onChange={(e) => { setName(e.target.value); setFormErr(null); }}
              />
            </FormField>
            {!direct && slugField}
            {direct && (
              <>
                <FormField
                  label="API format"
                  htmlFor="np-format"
                  w="md"
                  descId="np-format-desc"
                  help={apiFormat === "anthropic"
                    ? "The API the provider speaks. The credential is sent as Authorization: Bearer. For an API that expects x-api-key, set Auth header and Auth format ({key}) under Edit upstream after creating."
                    : "The API the provider speaks."}
                >
                  <Select
                    id="np-format"
                    aria-describedby="np-format-desc"
                    value={apiFormat}
                    onChange={(v) => { setApiFormat(v as ApiFormat); setFormErr(null); }}
                    options={FORMAT_OPTIONS}
                  />
                </FormField>
                <FormField
                  label="Credential slot"
                  htmlFor="np-slot"
                  w="md"
                  error={slotMessage ? <span id="np-slot-err">{slotMessage}</span> : undefined}
                  help={<span id="np-slot-help">{CREDENTIAL_SLOT_HELP}</span>}
                >
                  <Combobox
                    id="np-slot"
                    mono
                    value={slot}
                    options={slotSuggestions(slot, known ?? [])}
                    emptyText={known?.length === 0 ? NO_CREDENTIAL_SLOTS : undefined}
                    maxLength={140}
                    required
                    invalid={!!slotMessage}
                    autoCapitalize="characters"
                    aria-describedby={slotMessage ? "np-slot-err" : "np-slot-help"}
                    onChange={(v) => { setSlot(v.toUpperCase()); setSlotErr(null); setFormErr(null); }}
                    onBlur={() => setSlotLeft(true)}
                  />
                </FormField>
                {/* A name the relay cannot have is not looked up. */}
                {known !== undefined && slotErr === null && slotProblem === null && (
                  <SlotStatus
                    slots={splitSlots(slot)}
                    known={known}
                    whileMissing="You can create the provider now. It shows as Not configured until the key is set."
                  />
                )}
                <FormField label="Billing" htmlFor="np-billing" w="md">
                  <Select
                    id="np-billing"
                    value={billing}
                    onChange={(v) => { setBilling(v as "metered" | "flat"); setFormErr(null); }}
                    options={BILLING_OPTIONS}
                  />
                </FormField>
              </>
            )}
            {!direct && eligible.length > 0 && (
              <FormField label="Service" htmlFor="np-service" w="md">
                <Select
                  id="np-service"
                  value={chosen}
                  onChange={(v) => { setServiceId(v); setFormErr(null); }}
                  options={eligible.map((s) => ({ value: s.id, label: s.name || s.id }))}
                  placeholder="Select a service…"
                />
              </FormField>
            )}
          </FormFieldGroup>
          {direct && (
            <Disclosure label="Advanced" open={advancedOpen} onOpenChange={setAdvanced}>
              <FormFieldGroup>
                {slugField}
                <FormField
                  label="Base URL"
                  htmlFor="np-base-url"
                  error={urlErr ? <span id="np-base-url-err">{urlErr}</span> : undefined}
                  help={<span id="np-base-url-help">The https URL of the API, up to and including its version path.</span>}
                >
                  <Input
                    id="np-base-url"
                    mono
                    type="url"
                    inputMode="url"
                    value={baseUrl}
                    maxLength={2048}
                    required
                    invalid={!!urlErr}
                    autoComplete="off"
                    autoCapitalize="none"
                    spellCheck={false}
                    placeholder="https://"
                    aria-describedby={urlErr ? "np-base-url-err" : "np-base-url-help"}
                    onChange={(e) => { setBaseUrl(e.target.value); setUrlErr(null); setFormErr(null); }}
                  />
                </FormField>
                <ConcurrencyField
                  id="np-concurrency"
                  value={concurrency}
                  error={concurrencyErr}
                  onChange={(v) => { setConcurrency(v); setConcurrencyErr(null); setFormErr(null); }}
                />
                {apiFormat === "openai" && (
                  <ResponsesCheckbox id="np-responses" checked={responses} onChange={(v) => { setResponses(v); setFormErr(null); }} />
                )}
              </FormFieldGroup>
            </Disclosure>
          )}
          {direct ? null : loadFailed ? (
            <ErrorNotice>Couldn't load the services to choose from.</ErrorNotice>
          ) : loaded && eligible.length === 0 ? (
            <ErrorNotice variant="info" role="status">
              No service can be used: it has to be an HTTP service in API-key mode that is not a provider yet.
            </ErrorNotice>
          ) : null}
          {!direct && (
            <div>
              <Button variant="ghost" size="sm" onClick={() => showExisting(false)}>Set up a model server step by step</Button>
            </div>
          )}
          {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
        </>
      )}
    </Dialog>
  );
}
