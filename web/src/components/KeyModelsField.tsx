import { useId } from "react";
import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { Checkbox } from "@/components/ds";
import type { AiModel, AiProvider } from "@/lib/contract";

export interface ModelEntry { value: string; label: string }

/**
 * The entries a key's model list can hold, in list order: every model, then
 * "everything from" each provider. `keep` entries the lists do not know (a model or
 * provider removed since) stay, so editing a key does not silently drop them.
 */
export function useModelEntries(keep: string[] = []) {
  // Same keys and fetches as the Models and the Providers page.
  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
  });
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const modelList = Array.isArray(models.data) ? models.data : [];
  const entries: ModelEntry[] = [
    ...modelList.map((m) => ({ value: m.name, label: m.name })),
    ...(Array.isArray(providers.data) ? providers.data : []).map((p) => ({ value: `${p.slug}/*`, label: `Everything from ${p.slug}` })),
  ];
  for (const k of keep) if (!entries.some((e) => e.value === k)) entries.push({ value: k, label: k });
  return { entries, models: modelList, loading: models.isLoading || providers.isLoading };
}

export interface KeyModelsFieldProps {
  restricted: boolean;
  onRestrictedChange: (restricted: boolean) => void;
  entries: ModelEntry[];
  loading: boolean;
  /** The ticked entries. */
  allowed: string[];
  onAllowedChange: (allowed: string[]) => void;
}

/** "All models" or "Only these models" with the entries to tick. */
export function KeyModelsField({ restricted, onRestrictedChange, entries, loading, allowed, onAllowedChange }: KeyModelsFieldProps) {
  const scopeId = useId();
  return (
    <>
      <div role="radiogroup" aria-labelledby={scopeId} className="col gap-2">
        <span id={scopeId} className="muted small">Models the key may use</span>
        {([
          [false, "All models"],
          [true, "Only these models"],
        ] as const).map(([value, label]) => (
          <label key={label} className="row row-center gap-2">
            <input
              type="radio"
              name={scopeId}
              checked={restricted === value}
              onChange={() => onRestrictedChange(value)}
            />
            <span>{label}</span>
          </label>
        ))}
      </div>
      {restricted && (
        <div className="col gap-2 key-models" role="group" aria-label="Allowed models">
          {loading ? (
            <p className="muted small">Loading models…</p>
          ) : entries.length === 0 ? (
            <p className="muted small">There are no models or providers to choose from yet.</p>
          ) : (
            <>
              {entries.map((e, i) => {
                const id = `${scopeId}-entry-${i}`;
                return (
                  <div key={e.value} className="row row-center gap-2">
                    <Checkbox
                      id={id}
                      checked={allowed.includes(e.value)}
                      onChange={(on) => onAllowedChange(on ? [...allowed, e.value] : allowed.filter((v) => v !== e.value))}
                    />
                    <label htmlFor={id}>{e.label}</label>
                  </div>
                );
              })}
              <p className="muted small">A model allows that name on <code>/openai/v1</code> and <code>/anthropic</code>.</p>
              <p className="muted small">
                Everything from a provider also allows the provider's own address <code>/ai/&lt;provider&gt;/…</code>.
              </p>
            </>
          )}
        </div>
      )}
    </>
  );
}
