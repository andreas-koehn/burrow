import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, X } from "lucide-react";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Checkbox, Combobox, Dialog, Disclosure, ErrorNotice, FormField, FormFieldGroup, Input, Select, SubmitHint, Switch, firstMissing } from "@/components/ds";
import { modelNameError } from "@/lib/modelNames";
import { providerState } from "@/lib/status";
import type { AiModel, AiModelTarget, AiProvider, AiProviderModel, Dialect } from "@/lib/contract";

export interface ModelDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The model to edit; left out, the dialog creates one. */
  model?: AiModel;
  /** A new model only: the provider its first target starts on, in the format that provider speaks. */
  initialTarget?: { dialect: Dialect; provider: string };
}

// short names the format in a button's label: both lists are on screen at once.
const FORMATS: { dialect: Dialect; legend: string; short: string }[] = [
  { dialect: "openai", legend: "OpenAI format", short: "OpenAI" },
  { dialect: "anthropic", legend: "Anthropic format", short: "Anthropic" },
];

/** As many targets as the relay takes per format. */
const MAX_TARGETS = 8;

// Keys of the list entries; only ever compared with each other.
let lastKey = 0;
const newKey = () => ++lastKey;

/** One entry of a format's list. provider "" is unset: "Not served" when it is the only entry. */
interface TargetChoice {
  /** Stable across reorders, so a row keeps its fields and the focus. */
  key: number;
  provider: string;
  /** The provider's own id for the model: one from its catalog, or typed. */
  model: string;
}

interface TargetRowProps {
  dialect: Dialect;
  legend: string;
  short: string;
  /** 1-based place in the format's list. */
  n: number;
  count: number;
  providers: AiProvider[];
  value: TargetChoice;
  /** Receives the row's group element, so the list can move the focus with the row. */
  groupRef: (el: HTMLFieldSetElement | null) => void;
  onChange: (next: TargetChoice) => void;
  onMove: (by: -1 | 1) => void;
  onRemove: () => void;
  /** Said to a screen reader once a sync has filled the catalog. */
  announce: (message: string) => void;
}

function TargetRow({ dialect, legend, short, n, count, providers, value, groupRef, onChange, onMove, onRemove, announce }: TargetRowProps) {
  const qc = useQueryClient();
  const id = `model-target-${dialect}-${value.key}`;
  // Same key as ProviderModelsPanel: the provider's catalog.
  const catalog = useQuery({
    queryKey: ["ai", "provider-models", value.provider],
    queryFn: () => apiFetch<AiProviderModel[]>(`/ai/providers/${value.provider}/models`),
    retry: false,
    enabled: value.provider !== "",
  });
  const entries = Array.isArray(catalog.data) ? catalog.data : [];
  // The relay reads a catalog from a hosted provider's upstream, with its
  // credential; for a tunnel provider it answers 409, whatever its state.
  const provider = providers.find((p) => p.slug === value.provider);
  const canSync = provider?.kind === "direct" && providerState(provider) === "ready";
  const [syncErr, setSyncErr] = useState<string | null>(null);
  const sync = useMutation({
    mutationFn: (slug: string) => apiFetch<{ count: number }>(`/ai/providers/${slug}/models/sync`, { method: "POST" }),
    onMutate: () => setSyncErr(null),
    onSuccess: async (r, slug) => {
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["ai", "provider-models", slug] }),
        qc.invalidateQueries({ queryKey: ["ai", "provider", slug], exact: true }),
        qc.invalidateQueries({ queryKey: ["ai", "providers"] }),
      ]);
      announce(`Catalog synced: ${r.count} ${r.count === 1 ? "model" : "models"}.`);
    },
    // The answer of a sync for a provider the row has left is not this row's to show.
    onError: (e: unknown, slug) => {
      if (slug === value.provider) setSyncErr(e instanceof ApiError ? e.message : "Couldn't sync the catalog.");
    },
  });

  return (
    // tabIndex -1: after a move the focus goes to the row, whose name says where it is now.
    <fieldset className="target-row" aria-label={`${legend}, target ${n}`} tabIndex={-1} ref={groupRef}>
      {count > 1 && (
        <div className="target-row-actions">
          <span className="muted small" aria-hidden="true">{n}.</span>
          {n > 1 && (
            <button type="button" className="icon-btn" aria-label={`Move ${short} target ${n} up`} onClick={() => onMove(-1)}>
              <ArrowUp size={14} aria-hidden="true" />
            </button>
          )}
          {n < count && (
            <button type="button" className="icon-btn" aria-label={`Move ${short} target ${n} down`} onClick={() => onMove(1)}>
              <ArrowDown size={14} aria-hidden="true" />
            </button>
          )}
          <button type="button" className="icon-btn" aria-label={`Remove ${short} target ${n}`} onClick={onRemove}>
            <X size={14} aria-hidden="true" />
          </button>
        </div>
      )}
      <FormFieldGroup>
        <FormField label="Provider" htmlFor={`${id}-provider`} w="md">
          <Select
            id={`${id}-provider`}
            value={value.provider}
            onChange={(v) => { setSyncErr(null); onChange({ ...value, provider: v, model: "" }); }}
            placeholder="Select a provider…"
            options={[
              // A format is switched off on its only entry; a further one is removed instead.
              ...(count === 1 ? [{ value: "", label: "Not served" }] : []),
              ...providers.map((p) => ({ value: p.slug, label: p.slug })),
              // A target whose provider is not in the list any more stays visible.
              ...(value.provider !== "" && !providers.some((p) => p.slug === value.provider)
                ? [{ value: value.provider, label: value.provider }]
                : []),
            ]}
          />
        </FormField>
        {value.provider !== "" && (catalog.isLoading ? (
          <p className="muted small">Loading the catalog…</p>
        ) : (
          <>
            <FormField
              label="Target model id"
              htmlFor={`${id}-model-id`}
              w="md"
              descId={`${id}-model-desc`}
              error={syncErr ?? undefined}
              help={entries.length === 0
                ? "This provider's catalog is empty. Type the model id."
                : "The provider's own id for the model: one from its catalog, or typed."}
            >
              <Combobox
                id={`${id}-model-id`}
                mono
                value={value.model}
                // The label starts with the id: Firefox matches the text against the label and shows only that.
                options={entries.map((m) => ({ value: m.id, label: m.display_name ? `${m.id} · ${m.display_name}` : m.id }))}
                maxLength={200}
                aria-describedby={`${id}-model-desc`}
                onChange={(v) => onChange({ ...value, model: v })}
              />
            </FormField>
            {entries.length === 0 && canSync && (
              <div>
                <Button variant="secondary" size="sm" disabled={sync.isPending} onClick={() => sync.mutate(value.provider)}>
                  {sync.isPending ? "Syncing…" : "Sync catalog"}
                </Button>
              </div>
            )}
          </>
        ))}
      </FormFieldGroup>
    </fieldset>
  );
}

interface TargetListProps {
  dialect: Dialect;
  legend: string;
  short: string;
  /** undefined while the providers are still loading. */
  providers: AiProvider[] | undefined;
  value: TargetChoice[];
  onChange: (next: TargetChoice[]) => void;
  /** Said to a screen reader after a move or a removal. */
  announce: (message: string) => void;
}

/** What a target is called when it is announced. */
function targetName(t: TargetChoice, n: number): string {
  return t.provider !== "" && t.model.trim() !== "" ? `${t.provider}/${t.model.trim()}` : `Target ${n}`;
}

/** The ordered targets of one format: edit, add, remove, move up and down. */
function TargetList({ dialect, legend, short, providers, value, onChange, announce }: TargetListProps) {
  const rows = useRef(new Map<number, HTMLFieldSetElement>());
  // The row to focus once the list has rendered in its new order.
  const focusNext = useRef<number | null>(null);
  useEffect(() => {
    if (focusNext.current === null) return;
    rows.current.get(focusNext.current)?.focus();
    focusNext.current = null;
  });
  const setFocusKey = (key: number | null) => { focusNext.current = key; };

  const speaking = (providers ?? []).filter((p) => p.api_format === dialect);
  const served = value.some((t) => t.provider !== "");
  // A fallback is offered once there is something to fall back from.
  const complete = value.some((t) => t.provider !== "" && t.model.trim() !== "");

  function move(i: number, by: -1 | 1) {
    const to = i + by;
    const moved = value[i];
    const other = value[to];
    if (!moved || !other) return;
    const next = [...value];
    next[i] = other;
    next[to] = moved;
    onChange(next);
    // The button that was pressed may be gone at the end of the list: the row takes the focus.
    setFocusKey(moved.key);
    announce(`${targetName(moved, i + 1)} is now target ${to + 1} of ${value.length} in the ${legend}.`);
  }

  function remove(i: number) {
    const gone = value[i];
    if (!gone || value.length < 2) return;
    const next = value.filter((_, at) => at !== i);
    onChange(next);
    setFocusKey((next[i] ?? next[i - 1])?.key ?? null);
    announce(`${targetName(gone, i + 1)} removed from the ${legend}.`);
  }

  return (
    <fieldset className="target-group">
      <legend>{legend}</legend>
      {providers === undefined && !served ? (
        // A stored list is shown at once; the providers to choose from follow.
        <p className="muted small">Loading providers…</p>
      ) : speaking.length === 0 && !served ? (
        <p className="muted small">
          <span>No provider speaks this format yet.</span>{" "}
          <Link className="link-inline" to="/gateway/providers?new=local">Add a provider</Link>.
        </p>
      ) : (
        <>
          {served && (
            <p className="muted small">Tried in this order. Burrow moves on when a target fails before it has started answering.</p>
          )}
          {value.map((t, i) => (
            <TargetRow
              key={t.key}
              dialect={dialect}
              legend={legend}
              short={short}
              n={i + 1}
              count={value.length}
              providers={speaking}
              value={t}
              groupRef={(el) => { if (el) rows.current.set(t.key, el); else rows.current.delete(t.key); }}
              onChange={(next) => onChange(value.map((x) => (x.key === t.key ? next : x)))}
              onMove={(by) => move(i, by)}
              onRemove={() => remove(i)}
              announce={announce}
            />
          ))}
          {complete && (
            <Button
              variant="secondary"
              size="sm"
              disabled={value.length >= MAX_TARGETS}
              onClick={() => {
                const key = newKey();
                onChange([...value, { key, provider: "", model: "" }]);
                setFocusKey(key);
              }}
            >
              Add fallback target
            </Button>
          )}
          {value.length >= MAX_TARGETS && <p className="muted small">A format takes at most {MAX_TARGETS} targets.</p>}
        </>
      )}
    </fieldset>
  );
}

/** The targets of one format, in their order, reduced to what is compared. */
function ofFormat(targets: AiModelTarget[], dialect: Dialect): [string, string][] {
  return targets.filter((t) => t.dialect === dialect).map((t) => [t.provider, t.model]);
}

/** A timeout field's value in seconds, or null when it is not a whole number from 1 to 600 (the relay's rule). */
function parseTimeout(text: string): number | null {
  if (!/^\d{1,3}$/.test(text.trim())) return null;
  const n = Number(text.trim());
  return n >= 1 && n <= 600 ? n : null;
}

const TIMEOUT_RANGE = "Between 1 and 600 seconds.";

function ModelForm({ onOpenChange, model, initialTarget }: Omit<ModelDialogProps, "open">) {
  const qc = useQueryClient();
  const editing = model !== undefined;
  const [name, setName] = useState(model?.name ?? "");
  const [description, setDescription] = useState(model?.description ?? "");
  const [enabled, setEnabled] = useState(model?.enabled ?? true);
  const [choices, setChoices] = useState<Record<Dialect, TargetChoice[]>>(() => {
    const initial = (dialect: Dialect): TargetChoice[] => {
      const stored = (model?.targets ?? []).filter((t) => t.dialect === dialect);
      // A format always shows one entry; unset, it reads "Not served". A new
      // model can start on a provider: its model is still to be chosen.
      const first = !model && initialTarget?.dialect === dialect ? initialTarget.provider : "";
      const list = stored.length > 0 ? stored : [{ provider: first, model: "" }];
      return list.map((t) => ({ key: newKey(), provider: t.provider, model: t.model }));
    };
    return { openai: initial("openai"), anthropic: initial("anthropic") };
  });
  const [translate, setTranslate] = useState(model?.translate ?? false);
  const [onRateLimit, setOnRateLimit] = useState(model?.fallback_on_rate_limit ?? false);
  const [attemptText, setAttemptText] = useState(String(model?.attempt_timeout_s ?? 60));
  const [totalText, setTotalText] = useState(String(model?.total_timeout_s ?? 120));
  // Advanced starts open where the model holds something set there.
  const [advanced, setAdvanced] = useState(() => model !== undefined && (
    model.description !== "" || !model.enabled || model.fallback_on_rate_limit
    || model.attempt_timeout_s !== 60 || model.total_timeout_s !== 120
  ));
  const [lostOpen, setLostOpen] = useState(false);
  // A field's rule is named once the field was left; an unfinished target once the form was sent.
  const [nameLeft, setNameLeft] = useState(false);
  const [timeoutsLeft, setTimeoutsLeft] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [announcement, setAnnouncement] = useState("");
  const [formErr, setFormErr] = useState<string | null>(null);

  // A live region is read when its text changes. The same words twice in a
  // row (two unnamed entries removed from the same place) would not be read
  // again, so the region is emptied first and filled a moment later.
  const announceTimer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(announceTimer.current), []);
  const announce = (message: string) => {
    window.clearTimeout(announceTimer.current);
    setAnnouncement("");
    announceTimer.current = window.setTimeout(() => setAnnouncement(message), 100);
  };

  // Same key and fetch as the Providers page.
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const providerList = Array.isArray(providers.data) ? providers.data : providers.isLoading ? undefined : [];

  // What is sent: OpenAI first, each format in the order arranged here. A
  // lone unset entry is a format that is not served.
  const targets: AiModelTarget[] = FORMATS.flatMap(({ dialect }) => choices[dialect]
    .filter((t) => t.provider !== "" && t.model.trim() !== "")
    .map((t) => ({ dialect, provider: t.provider, model: t.model.trim() })));
  // An entry with a provider but no model id, or a fallback entry left empty,
  // is unfinished: the first one, by the name its group carries.
  const unfinished = FORMATS.flatMap(({ dialect, legend }) => choices[dialect]
    .map((t, i, list) => ((t.provider === "" ? list.length > 1 : t.model.trim() === "") ? `${legend}, target ${i + 1}` : null)))
    .find((name) => name !== null) ?? null;
  // The relay refuses the same provider and model twice in one format.
  const twice = targets.some((t, i) => targets.findIndex((x) => x.dialect === t.dialect && x.provider === t.provider && x.model === t.model) !== i);
  const nameErr = modelNameError(name)
    ?? ((providerList ?? []).some((p) => p.slug === name) ? "A provider already uses this name." : null);

  const attempt = parseTimeout(attemptText);
  const total = parseTimeout(totalText);
  const attemptErr = attempt === null ? TIMEOUT_RANGE : null;
  const totalErr = total === null ? TIMEOUT_RANGE
    : attempt !== null && total < attempt ? "The total timeout must not be shorter than the attempt timeout."
    : null;
  const timeoutsOk = attemptErr === null && totalErr === null;

  const payload = {
    name,
    description: description.trim(),
    enabled,
    fallback_on_rate_limit: onRateLimit,
    translate,
    attempt_timeout_s: attempt ?? 0,
    total_timeout_s: total ?? 0,
    targets,
  };
  const dirty = !model
    || name !== model.name
    || payload.description !== model.description
    || enabled !== model.enabled
    || onRateLimit !== model.fallback_on_rate_limit
    || translate !== (model.translate ?? false)
    || attempt !== model.attempt_timeout_s
    || total !== model.total_timeout_s
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

  // Beside the button while it is disabled, the first of these that applies.
  const missing = [
    name === "" && "Enter a name.",
    nameErr !== null && "Check the model name.",
    targets.length === 0 && "Choose a provider and a model for at least one format.",
    twice && "A target is listed twice.",
    !timeoutsOk && "Check the timeouts under Advanced.",
    !dirty && "No changes to save.",
  ];
  const hint = firstMissing(missing);
  const change = <T,>(set: (v: T) => void) => (v: T) => { set(v); setFormErr(null); };
  // A field with an error is never left hidden.
  const advancedOpen = advanced || !timeoutsOk;
  const shownNameErr = nameLeft ? nameErr : null;
  const shownAttemptErr = timeoutsLeft ? attemptErr : null;
  const shownTotalErr = timeoutsLeft ? totalErr : null;

  // An unfinished target does not disable the button: it is named when the form is sent.
  function submit() {
    setFormErr(null);
    if (unfinished !== null) { setSubmitted(true); return; }
    save.mutate();
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!save.isPending) onOpenChange(o); }}
      title={model ? `Edit ${model.name}` : "New model"}
      description="A name apps send as the model, and where each API format sends it."
      size="md"
      footer={
        <>
          <SubmitHint id="model-hint" missing={missing} />
          <Button variant="secondary" disabled={save.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={hint !== undefined || save.isPending}
            aria-describedby={hint !== undefined ? "model-hint" : undefined}
            onClick={submit}
          >
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
          error={shownNameErr ?? undefined}
          help={model && name !== model.name
            ? "Gateway keys that list the old name are not updated."
            : "What apps send as the model, e.g. burrow-medium."}
        >
          <Input
            id="model-name"
            mono
            value={name}
            invalid={shownNameErr !== null}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={63}
            onChange={(e) => change(setName)(e.target.value)}
            onBlur={() => setNameLeft(true)}
          />
        </FormField>
      </FormFieldGroup>

      {FORMATS.map(({ dialect, legend, short }) => (
        <TargetList
          key={dialect}
          dialect={dialect}
          legend={legend}
          short={short}
          providers={providerList}
          value={choices[dialect]}
          onChange={(next) => { setChoices((c) => ({ ...c, [dialect]: next })); setFormErr(null); setSubmitted(false); }}
          announce={announce}
        />
      ))}
      {/* Moves and removals, for a screen reader. */}
      <p className="visually-hidden" role="status">{announcement}</p>
      {submitted && unfinished !== null && (
        <ErrorNotice>{unfinished} needs a provider and a model. Remove it if you do not need it.</ErrorNotice>
      )}

      <div className="model-translate">
        <label className="row row-center gap-2">
          <Switch
            aria-label="Translate for the other format"
            aria-describedby="model-translate-help"
            checked={translate}
            onChange={change(setTranslate)}
          />
          <span>Translate for the other format</span>
        </label>
        <p id="model-translate-help" className="muted small">
          Answer the other format by translating to this model's targets.
        </p>
        <Disclosure label="What gets lost" open={lostOpen} onOpenChange={setLostOpen}>
          <p className="muted small">
            A format is served natively where it has a target. With this on, a format without a target is answered by
            translating to and from the targets of the other format, and the Responses API is answered through an
            OpenAI-format target that does not offer it. A target of its own format is always used first.
          </p>
          <p className="muted small">
            Translated requests lose what the other format cannot express: prompt caching hints, thinking signatures and
            beta features are not carried over. Every translated response names what was left out.
          </p>
        </Disclosure>
      </div>

      <Disclosure label="Advanced" open={advancedOpen} onOpenChange={setAdvanced}>
        <FormFieldGroup>
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
          <div className="row row-center gap-2">
            <Checkbox id="model-on-rate-limit" checked={onRateLimit} onChange={change(setOnRateLimit)} describedBy="model-on-rate-limit-help" />
            <label htmlFor="model-on-rate-limit">Also fall back when a provider rate-limits (429)</label>
          </div>
          <p id="model-on-rate-limit-help" className="muted small">
            Off, a 429 goes back to the app as it is.
          </p>
          <FormField
            label="Attempt timeout (seconds)"
            htmlFor="model-attempt-timeout"
            w="sm"
            error={shownAttemptErr ?? undefined}
            help="How long one target may take to start answering before the next one is tried."
          >
            <Input
              id="model-attempt-timeout"
              type="number"
              inputMode="numeric"
              min={1}
              max={600}
              step={1}
              value={attemptText}
              invalid={shownAttemptErr !== null}
              onChange={(e) => change(setAttemptText)(e.target.value)}
              onBlur={() => setTimeoutsLeft(true)}
            />
          </FormField>
          <FormField
            label="Total timeout (seconds)"
            htmlFor="model-total-timeout"
            w="sm"
            error={shownTotalErr ?? undefined}
            help="How long all attempts together may take before the request fails."
          >
            <Input
              id="model-total-timeout"
              type="number"
              inputMode="numeric"
              min={1}
              max={600}
              step={1}
              value={totalText}
              invalid={shownTotalErr !== null}
              onChange={(e) => change(setTotalText)(e.target.value)}
              onBlur={() => setTimeoutsLeft(true)}
            />
          </FormField>
        </FormFieldGroup>
      </Disclosure>
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}

/**
 * Create or edit a synthetic model: its name, per API format the ordered list
 * of targets, and how it falls back. The form starts fresh every time it opens.
 */
export function ModelDialog({ open, onOpenChange, model, initialTarget }: ModelDialogProps) {
  if (!open) return null;
  return <ModelForm onOpenChange={onOpenChange} model={model} initialTarget={initialTarget} />;
}
