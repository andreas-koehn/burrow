import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, Select, Switch } from "@/components/ds";
import { modelNameError } from "@/lib/modelNames";
import type { AiModel, AiModelTarget, AiProvider, AiProviderModel, Dialect } from "@/lib/contract";

export interface ModelDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The model to edit; left out, the dialog creates one. */
  model?: AiModel;
}

const FORMATS: { dialect: Dialect; legend: string }[] = [
  { dialect: "openai", legend: "OpenAI format" },
  { dialect: "anthropic", legend: "Anthropic format" },
];

/** Where one format is sent. provider "" is "not served". */
interface TargetChoice {
  provider: string;
  model: string;
  /** The model id is typed, not picked from the provider's list. */
  manual: boolean;
}

interface TargetGroupProps {
  dialect: Dialect;
  legend: string;
  /** undefined while the providers are still loading. */
  providers: AiProvider[] | undefined;
  value: TargetChoice;
  onChange: (next: TargetChoice) => void;
}

function TargetGroup({ dialect, legend, providers, value, onChange }: TargetGroupProps) {
  const id = `model-target-${dialect}`;
  // Same key as ProviderModelsPanel: the provider's stored model list.
  const catalog = useQuery({
    queryKey: ["ai", "provider-models", value.provider],
    queryFn: () => apiFetch<AiProviderModel[]>(`/ai/providers/${value.provider}/models`),
    retry: false,
    enabled: value.provider !== "",
  });
  const ids = Array.isArray(catalog.data) ? catalog.data.map((m) => m.id) : [];
  const speaking = (providers ?? []).filter((p) => p.api_format === dialect);
  // A model id the list does not have can only be shown in the text field.
  const typed = value.manual || ids.length === 0 || (value.model !== "" && !ids.includes(value.model));

  return (
    <fieldset className="target-group">
      <legend>{legend}</legend>
      {providers === undefined ? (
        <p className="muted small">Loading providers…</p>
      ) : speaking.length === 0 && value.provider === "" ? (
        <p className="muted small">
          <span>No provider speaks this format yet.</span>{" "}
          Add one under <Link className="link-inline" to="/gateway/providers">Providers</Link>.
        </p>
      ) : (
        <FormFieldGroup>
          <FormField label="Provider" htmlFor={`${id}-provider`} w="md">
            <Select
              id={`${id}-provider`}
              value={value.provider}
              onChange={(v) => onChange({ provider: v, model: "", manual: false })}
              options={[
                { value: "", label: "Not served" },
                ...speaking.map((p) => ({ value: p.slug, label: p.slug })),
                // A target whose provider is not in the list any more stays visible.
                ...(value.provider !== "" && !speaking.some((p) => p.slug === value.provider)
                  ? [{ value: value.provider, label: value.provider }]
                  : []),
              ]}
            />
          </FormField>
          {value.provider !== "" && (catalog.isLoading ? (
            <p className="muted small">Loading models…</p>
          ) : typed ? (
            <FormField
              label="Target model id"
              htmlFor={`${id}-model-id`}
              w="md"
              help={ids.length === 0 ? "This provider has no synced models. Type the model id." : "The provider's own id for the model."}
            >
              <Input
                id={`${id}-model-id`}
                mono
                value={value.model}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                maxLength={200}
                onChange={(e) => onChange({ ...value, model: e.target.value })}
              />
              {ids.length > 0 && (
                <button type="button" className="link small" onClick={() => onChange({ ...value, model: "", manual: false })}>
                  Choose from the list instead
                </button>
              )}
            </FormField>
          ) : (
            <FormField label="Target model" htmlFor={`${id}-model`} w="md">
              <Select
                id={`${id}-model`}
                value={value.model}
                onChange={(v) => onChange({ ...value, model: v })}
                options={ids.map((m) => ({ value: m, label: m }))}
                placeholder="Select a model…"
              />
              <button type="button" className="link small" onClick={() => onChange({ ...value, model: "", manual: true })}>
                Enter a model id instead
              </button>
            </FormField>
          ))}
        </FormFieldGroup>
      )}
    </fieldset>
  );
}

/** The targets of one format, in their order, reduced to what is compared. */
function ofFormat(targets: AiModelTarget[], dialect: Dialect): [string, string][] {
  return targets.filter((t) => t.dialect === dialect).map((t) => [t.provider, t.model]);
}

function initialChoice(model: AiModel | undefined, dialect: Dialect): TargetChoice {
  const first = model?.targets.find((t) => t.dialect === dialect);
  return { provider: first?.provider ?? "", model: first?.model ?? "", manual: false };
}

/**
 * The targets the form stands for, OpenAI first. The dialog edits the first
 * target of each format; further ones (set through the API) are kept behind it.
 */
function targetsOf(choices: Record<Dialect, TargetChoice>, original: AiModel | undefined): AiModelTarget[] {
  const out: AiModelTarget[] = [];
  for (const { dialect } of FORMATS) {
    const c = choices[dialect];
    const id = c.model.trim();
    if (c.provider === "" || id === "") continue;
    out.push({ dialect, provider: c.provider, model: id });
    const rest = (original?.targets ?? []).filter((t) => t.dialect === dialect).slice(1);
    out.push(...rest.filter((t) => !(t.provider === c.provider && t.model === id)));
  }
  return out;
}

function ModelForm({ onOpenChange, model }: Omit<ModelDialogProps, "open">) {
  const qc = useQueryClient();
  const editing = model !== undefined;
  const [name, setName] = useState(model?.name ?? "");
  const [description, setDescription] = useState(model?.description ?? "");
  const [enabled, setEnabled] = useState(model?.enabled ?? true);
  const [choices, setChoices] = useState<Record<Dialect, TargetChoice>>({
    openai: initialChoice(model, "openai"),
    anthropic: initialChoice(model, "anthropic"),
  });
  const [formErr, setFormErr] = useState<string | null>(null);

  // Same key and fetch as the Providers page.
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const providerList = Array.isArray(providers.data) ? providers.data : providers.isLoading ? undefined : [];

  const targets = targetsOf(choices, model);
  // A format with a provider but no model id is unfinished, not "not served".
  const unfinished = FORMATS.some(({ dialect }) => choices[dialect].provider !== "" && choices[dialect].model.trim() === "");
  const nameErr = modelNameError(name)
    ?? ((providerList ?? []).some((p) => p.slug === name) ? "A provider already uses this name." : null);

  const payload = {
    name,
    description: description.trim(),
    enabled,
    // The dialog does not edit these; an existing model keeps what it has.
    ...(model ? {
      fallback_on_rate_limit: model.fallback_on_rate_limit,
      attempt_timeout_s: model.attempt_timeout_s,
      total_timeout_s: model.total_timeout_s,
    } : {}),
    targets,
  };
  const dirty = !model
    || name !== model.name
    || payload.description !== model.description
    || enabled !== model.enabled
    // Per format: the relay lists a model's targets by format, the form OpenAI first.
    || FORMATS.some(({ dialect }) => JSON.stringify(ofFormat(targets, dialect)) !== JSON.stringify(ofFormat(model.targets, dialect)));

  const save = useMutation({
    mutationFn: () => model
      ? apiFetch<AiModel>(`/ai/models/${encodeURIComponent(model.name)}`, { method: "PUT", body: JSON.stringify(payload) })
      : apiFetch<AiModel>("/ai/models", { method: "POST", body: JSON.stringify(payload) }),
    onSuccess: async () => {
      toast.success(editing ? "Model saved." : "Model created.");
      onOpenChange(false);
      // A provider's row names the first model that targets it.
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["ai", "models"] }),
        qc.invalidateQueries({ queryKey: ["ai", "providers"] }),
      ]);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setFormErr("Couldn't save the model.");
      else if (e.status === 403) setFormErr("You don't have permission to change models.");
      else setFormErr(e.message);
    },
  });

  const ready = name !== "" && nameErr === null && targets.length > 0 && !unfinished && dirty && !save.isPending;
  const change = <T,>(set: (v: T) => void) => (v: T) => { set(v); setFormErr(null); };

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!save.isPending) onOpenChange(o); }}
      title={model ? `Edit ${model.name}` : "New model"}
      description="A name clients send as the model, and where each API format sends it."
      size="md"
      footer={
        <>
          <Button variant="secondary" disabled={save.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!ready} onClick={() => save.mutate()}>
            {save.isPending ? "Saving…" : editing ? "Save changes" : "Create model"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField
          label="Model name"
          htmlFor="model-name"
          w="md"
          error={nameErr ?? undefined}
          help={model && name !== model.name
            ? "Gateway keys that list the old name are not updated."
            : "What clients send as the model, e.g. burrow-medium."}
        >
          <Input
            id="model-name"
            mono
            value={name}
            invalid={nameErr !== null}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={63}
            onChange={(e) => change(setName)(e.target.value)}
          />
        </FormField>
        <FormField label="Description (optional)" htmlFor="model-description" w="md">
          <Input
            id="model-description"
            value={description}
            maxLength={500}
            onChange={(e) => change(setDescription)(e.target.value)}
          />
        </FormField>
        <label className="row row-center gap-2">
          <Switch aria-label="Enabled" checked={enabled} onChange={change(setEnabled)} />
          <span>Enabled</span>
        </label>
      </FormFieldGroup>

      {FORMATS.map(({ dialect, legend }) => (
        <TargetGroup
          key={dialect}
          dialect={dialect}
          legend={legend}
          providers={providerList}
          value={choices[dialect]}
          onChange={(next) => { setChoices((c) => ({ ...c, [dialect]: next })); setFormErr(null); }}
        />
      ))}
      {targets.length === 0 ? (
        <p className="muted small">Choose a target for at least one format.</p>
      ) : unfinished && (
        <p className="muted small">A format with a provider needs a target model, or set its provider to "Not served".</p>
      )}
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}

/**
 * Create or edit a synthetic model. One target per API format here; the form
 * starts fresh every time it opens.
 */
export function ModelDialog({ open, onOpenChange, model }: ModelDialogProps) {
  if (!open) return null;
  return <ModelForm onOpenChange={onOpenChange} model={model} />;
}
