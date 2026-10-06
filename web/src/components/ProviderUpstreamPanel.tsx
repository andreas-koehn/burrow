import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Checkbox, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, Select } from "@/components/ds";
import { credentialSlotError, envVarForSlot } from "@/lib/providerPresets";
import type { AiProvider, AiProviderUpstreamInput } from "@/lib/contract";

const BILLING_LABEL: Record<AiProvider["billing"], string> = { metered: "Metered", flat: "Flat rate" };
const BILLING_OPTIONS = [
  { value: "metered", label: BILLING_LABEL.metered },
  { value: "flat", label: BILLING_LABEL.flat },
];
const HEADER_NAME_RE = /^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$/;
const EXTRA_FORMAT = "One header per line, as Name: value.";

/** Parses "Name: value" lines; null when a line is not one. */
function parseExtraHeaders(text: string): Record<string, string> | null {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    const i = line.indexOf(":");
    const name = i < 0 ? "" : line.slice(0, i).trim();
    if (!HEADER_NAME_RE.test(name)) return null;
    out[name] = line.slice(i + 1).trim();
  }
  return out;
}

export interface ResponsesCheckboxProps {
  id: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
}

/**
 * The operator's statement that a provider offers the OpenAI Responses API.
 * Nothing probes the upstream; the gateway refuses the endpoint while it is off.
 */
export function ResponsesCheckbox({ id, checked, onChange }: ResponsesCheckboxProps) {
  return (
    <div className="col gap-2">
      <div className="row row-center gap-2">
        <Checkbox id={id} checked={checked} onChange={onChange} describedBy={`${id}-help`} />
        <label htmlFor={id}>Offers the Responses API (needed by Codex)</label>
      </div>
      <p id={`${id}-help`} className="muted small">
        Leave off unless the provider documents <code>POST /responses</code>. Requests to that endpoint are
        refused for this provider while it is off.
      </p>
    </div>
  );
}

export interface ProviderResponsesSettingProps {
  /** A provider without upstream settings (kind "tunnel") in the OpenAI format. */
  provider: AiProvider;
  isAdmin: boolean;
}

/**
 * Whether a tunnelled provider offers the Responses API. It is saved at once,
 * through the provider's own update; slug and name are sent back unchanged.
 */
export function ProviderResponsesSetting({ provider, isAdmin }: ProviderResponsesSettingProps) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: (on: boolean) =>
      apiFetch<AiProvider>(`/ai/providers/${provider.slug}`, {
        method: "PUT",
        body: JSON.stringify({ slug: provider.slug, name: provider.name, supports_responses: on }),
      }),
    onMutate: () => setError(null),
    onSuccess: async (next) => {
      qc.setQueryData(["ai", "provider", provider.slug], next);
      await qc.invalidateQueries({ queryKey: ["ai", "providers"] });
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setError("Couldn't save the setting.");
      else if (e.status === 403) setError("You don't have permission to change this provider.");
      else setError(e.message);
    },
  });
  // While a save is under way the box shows what was asked for.
  const checked = save.isPending ? save.variables : provider.supports_responses;
  return (
    <section className="card col gap-2" aria-label="Responses API">
      {isAdmin ? (
        <ResponsesCheckbox
          id="pr-responses"
          checked={checked}
          onChange={(on) => { if (!save.isPending) save.mutate(on); }}
        />
      ) : (
        <p className="muted small">
          {provider.supports_responses
            ? "This provider offers the Responses API."
            : "This provider does not offer the Responses API; requests to that endpoint are refused."}
        </p>
      )}
      {error && <ErrorNotice>{error}</ErrorNotice>}
    </section>
  );
}

export interface ProviderUpstreamPanelProps {
  /** A direct provider. Admins also get auth_header, auth_format and extra_header_names. */
  provider: AiProvider;
  isAdmin: boolean;
}

/**
 * Where the relay sends a direct provider's requests and whether its
 * credential is configured. The credential is set on the relay: this panel
 * shows and edits the name of its slot, never a value.
 */
export function ProviderUpstreamPanel({ provider, isAdmin }: ProviderUpstreamPanelProps) {
  const [editOpen, setEditOpen] = useState(false);
  const slot = provider.credential_slot;
  const extra = provider.extra_header_names;
  return (
    <section className="card" aria-label="Upstream">
      <div className="panel-head">
        <h2>Upstream</h2>
        {isAdmin && (
          <Button variant="secondary" size="sm" className="ml-auto" onClick={() => setEditOpen(true)}>Edit</Button>
        )}
      </div>
      <dl className="def-list">
        <div className="def-row">
          <dt className="def-key">Base URL</dt>
          <dd className="def-val" style={{ minWidth: 0, overflowWrap: "anywhere" }}>{provider.upstream_base_url}</dd>
        </div>
        <div className="def-row">
          <dt className="def-key">Credential slot</dt>
          <dd className="def-val">{slot}</dd>
        </div>
        <div className="def-row">
          <dt className="def-key">Credential</dt>
          <dd className="def-val">
            {provider.credential_present
              ? <Badge kind="status-connected">configured</Badge>
              : <Badge kind="status-idle">not configured</Badge>}
          </dd>
        </div>
        <div className="def-row">
          <dt className="def-key">Billing</dt>
          <dd className="def-val">{BILLING_LABEL[provider.billing]}</dd>
        </div>
        {provider.api_format === "openai" && (
          <div className="def-row">
            <dt className="def-key">Responses API</dt>
            <dd className="def-val">{provider.supports_responses ? "offered" : "not offered"}</dd>
          </div>
        )}
        {isAdmin && provider.auth_header !== undefined && (
          <div className="def-row">
            <dt className="def-key">Auth header</dt>
            <dd className="def-val">{provider.auth_header}</dd>
          </div>
        )}
        {isAdmin && provider.auth_format !== undefined && (
          <div className="def-row">
            <dt className="def-key">Auth format</dt>
            <dd className="def-val">{provider.auth_format}</dd>
          </div>
        )}
        {isAdmin && extra !== undefined && (
          <div className="def-row">
            <dt className="def-key">Extra headers</dt>
            <dd className="def-val" style={{ minWidth: 0, overflowWrap: "anywhere" }}>
              {extra.length > 0 ? extra.join(", ") : <span className="muted">none</span>}
            </dd>
          </div>
        )}
      </dl>
      {provider.credential_present ? (
        <p className="muted small">The credential itself is set on the relay and is never shown here.</p>
      ) : (
        <ErrorNotice variant="warn" role="note">
          Slot {slot} is not set on the relay, or is set to an empty value. Set{" "}
          <code>{envVarForSlot(slot)}</code> to a non-empty value in the relay's environment and restart it.
          Until then this provider answers 503.
        </ErrorNotice>
      )}
      {isAdmin && <EditUpstreamDialog provider={provider} open={editOpen} onOpenChange={setEditOpen} />}
    </section>
  );
}

interface EditUpstreamDialogProps {
  provider: AiProvider;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

function EditUpstreamDialog({ open, ...rest }: EditUpstreamDialogProps) {
  // Mounted per opening, so every opening starts from the stored settings.
  return open ? <EditUpstreamForm {...rest} /> : null;
}

type Field = "url" | "slot" | "authHeader" | "authFormat" | "extra";

function EditUpstreamForm({ provider, onOpenChange }: Omit<EditUpstreamDialogProps, "open">) {
  const qc = useQueryClient();
  const stored = {
    baseUrl: provider.upstream_base_url,
    slot: provider.credential_slot,
    billing: provider.billing,
    authHeader: provider.auth_header ?? "Authorization",
    authFormat: provider.auth_format ?? "Bearer {key}",
  };
  const names = provider.extra_header_names ?? [];
  const [baseUrl, setBaseUrl] = useState(stored.baseUrl);
  const [slot, setSlot] = useState(stored.slot);
  const [billing, setBilling] = useState(stored.billing);
  const [authHeader, setAuthHeader] = useState(stored.authHeader);
  const [authFormat, setAuthFormat] = useState(stored.authFormat);
  // The stored values are never returned, so the field starts empty: empty
  // keeps them, text replaces all of them, and removing them is its own choice.
  const [extraText, setExtraText] = useState("");
  const [clearExtra, setClearExtra] = useState(false);
  const [responses, setResponses] = useState(provider.supports_responses);
  // What the server said about one field of the last attempt; formErr is
  // everything else. Shown until the next edit of any field (one setting can
  // be refused because of another) and never a reason to disable Save: a
  // refusal such as a failed DNS lookup may pass on a second try.
  const [fieldErr, setFieldErr] = useState<Partial<Record<Field, string>>>({});
  const [formErr, setFormErr] = useState<string | null>(null);

  // Names of the credential slots set on the relay; never their values.
  const slots = useQuery({
    queryKey: ["upstream-credential-slots"],
    queryFn: () => apiFetch<{ slots: string[] }>("/upstream-credentials/slots"),
    retry: false,
  });

  const parsedExtra = clearExtra ? {} : extraText.trim() === "" ? undefined : parseExtraHeaders(extraText);
  const slotMessage = credentialSlotError(slot);
  // Only these block Save.
  const invalid: Partial<Record<Field, string>> = {
    ...(slotMessage ? { slot: slotMessage } : {}),
    ...(authFormat.split("{key}").length !== 2 ? { authFormat: "Must contain {key} exactly once." } : {}),
    ...(parsedExtra === null ? { extra: EXTRA_FORMAT } : {}),
  };
  const errors: Partial<Record<Field, string>> = { ...fieldErr, ...invalid };

  // Only what changed is sent: a field that is left out keeps its stored value.
  const body: AiProviderUpstreamInput = {
    ...(baseUrl.trim() !== stored.baseUrl ? { base_url: baseUrl.trim() } : {}),
    ...(slot !== stored.slot ? { credential_slot: slot } : {}),
    ...(billing !== stored.billing ? { billing } : {}),
    ...(authHeader.trim() !== stored.authHeader ? { auth_header: authHeader.trim() } : {}),
    ...(authFormat !== stored.authFormat ? { auth_format: authFormat } : {}),
    ...(parsedExtra ? { extra_headers: parsedExtra } : {}),
    ...(responses !== provider.supports_responses ? { supports_responses: responses } : {}),
  };
  const unchanged = Object.keys(body).length === 0;

  const save = useMutation({
    mutationFn: () =>
      apiFetch<AiProvider>(`/ai/providers/${provider.slug}/upstream`, { method: "PUT", body: JSON.stringify(body) }),
    // A retry without an edit must not keep the last refusal on screen.
    onMutate: () => edited(),
    onSuccess: async (next) => {
      qc.setQueryData(["ai", "provider", provider.slug], next);
      await qc.invalidateQueries({ queryKey: ["ai", "providers"] });
      onOpenChange(false);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) return setFormErr("Couldn't save the upstream settings.");
      if (e.status === 403) return setFormErr("You don't have permission to change upstream settings.");
      // The server's reasons start with the name of the setting they are about.
      const field: Field | null = e.status !== 400 ? null
        : e.message.startsWith("base URL") ? "url"
        : e.message.startsWith("credential slot") ? "slot"
        : e.message.startsWith("auth header") ? "authHeader"
        : e.message.startsWith("auth format") ? "authFormat"
        : /^(an |at most \d+ )?extra header/.test(e.message) ? "extra"
        : null;
      if (field) setFieldErr({ [field]: e.message });
      else setFormErr(e.message);
    },
  });

  function edited() {
    setFormErr(null);
    setFieldErr({});
  }

  const slotMissing = slot !== "" && !errors.slot && slots.data !== undefined && !slots.data.slots.includes(slot);
  const blocked = unchanged || baseUrl.trim() === "" || slot === "" || authHeader.trim() === ""
    || Object.keys(invalid).length > 0 || save.isPending;
  const extraHelp = names.length > 0
    ? `Set now: ${names.join(", ")}. Their values are never shown. Leave this empty to keep them; anything entered here replaces all of them. ${EXTRA_FORMAT}`
    : `None set. ${EXTRA_FORMAT} Values are never shown again after saving.`;
  const err = (field: Field, id: string) =>
    errors[field] ? <span id={`${id}-err`}>{errors[field]}</span> : undefined;

  return (
    <Dialog
      open
      size="md"
      onOpenChange={(o) => { if (!save.isPending) onOpenChange(o); }}
      title={`Edit upstream · ${provider.name}`}
      description="Where the relay sends this provider's requests. The credential is set on the relay; only the name of its slot is kept here."
      footer={
        <>
          <Button variant="secondary" disabled={save.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={blocked} onClick={() => save.mutate()}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField label="Base URL" htmlFor="eu-base-url" error={err("url", "eu-base-url")}>
          <Input
            id="eu-base-url"
            mono
            type="url"
            inputMode="url"
            value={baseUrl}
            maxLength={2048}
            required
            invalid={!!errors.url}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-describedby={errors.url ? "eu-base-url-err" : undefined}
            onChange={(e) => { setBaseUrl(e.target.value); edited(); }}
          />
        </FormField>
        <FormField
          label="Credential slot"
          htmlFor="eu-slot"
          w="md"
          error={err("slot", "eu-slot")}
          help={<span id="eu-slot-help">The name of the slot, not the key. The key is set on the relay and must be set to a non-empty value.</span>}
        >
          <Input
            id="eu-slot"
            mono
            value={slot}
            maxLength={32}
            required
            invalid={!!errors.slot}
            autoComplete="off"
            autoCapitalize="characters"
            spellCheck={false}
            aria-describedby={errors.slot ? "eu-slot-err" : "eu-slot-help"}
            onChange={(e) => { setSlot(e.target.value.toUpperCase()); edited(); }}
          />
        </FormField>
        <FormField label="Billing" htmlFor="eu-billing" w="md">
          <Select
            id="eu-billing"
            value={billing}
            onChange={(v) => { setBilling(v as AiProvider["billing"]); edited(); }}
            options={BILLING_OPTIONS}
          />
        </FormField>
        <FormField label="Auth header" htmlFor="eu-auth-header" w="md" error={err("authHeader", "eu-auth-header")}>
          <Input
            id="eu-auth-header"
            mono
            value={authHeader}
            maxLength={64}
            required
            invalid={!!errors.authHeader}
            autoComplete="off"
            spellCheck={false}
            aria-describedby={errors.authHeader ? "eu-auth-header-err" : undefined}
            onChange={(e) => { setAuthHeader(e.target.value); edited(); }}
          />
        </FormField>
        <FormField
          label="Auth format"
          htmlFor="eu-auth-format"
          w="md"
          error={err("authFormat", "eu-auth-format")}
          help={<span id="eu-auth-format-help">The header value. The relay puts the credential where {"{key}"} stands.</span>}
        >
          <Input
            id="eu-auth-format"
            mono
            value={authFormat}
            maxLength={128}
            required
            invalid={!!errors.authFormat}
            autoComplete="off"
            spellCheck={false}
            aria-describedby={errors.authFormat ? "eu-auth-format-err" : "eu-auth-format-help"}
            onChange={(e) => { setAuthFormat(e.target.value); edited(); }}
          />
        </FormField>
        <FormField
          label="Extra headers"
          htmlFor="eu-extra"
          error={errors.extra ? <span id="eu-extra-err">{errors.extra} {extraHelp}</span> : undefined}
          help={<span id="eu-extra-help">{extraHelp}</span>}
        >
          <textarea
            id="eu-extra"
            className={`input mono resizable${errors.extra ? " is-invalid" : ""}`}
            rows={3}
            value={clearExtra ? "" : extraText}
            disabled={clearExtra}
            autoComplete="off"
            spellCheck={false}
            aria-invalid={errors.extra ? true : undefined}
            aria-describedby={errors.extra ? "eu-extra-err" : "eu-extra-help"}
            placeholder="X-Title: Burrow"
            onChange={(e) => { setExtraText(e.target.value); edited(); }}
          />
        </FormField>
        {names.length > 0 && (
          <div className="row row-center gap-2">
            <Checkbox id="eu-extra-clear" checked={clearExtra} onChange={(v) => { setClearExtra(v); edited(); }} />
            <label htmlFor="eu-extra-clear">Remove all extra headers</label>
          </div>
        )}
        {provider.api_format === "openai" && (
          <ResponsesCheckbox id="eu-responses" checked={responses} onChange={(v) => { setResponses(v); edited(); }} />
        )}
      </FormFieldGroup>
      {slotMissing && (
        <ErrorNotice variant="info" role="note">
          Slot {slot} is not set on the relay. Set <code>{envVarForSlot(slot)}</code> to a non-empty value in
          the relay's environment and restart it. The provider answers 503 until the key is set.
        </ErrorNotice>
      )}
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}
