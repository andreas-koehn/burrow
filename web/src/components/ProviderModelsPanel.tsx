import { useId, useRef, useState } from "react";
import type { FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Trash2 } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, EmptyState, ErrorNotice, Input, SkeletonRows } from "@/components/ds";
import type { AiProviderModel } from "@/lib/contract";

export interface ProviderModelsPanelProps {
  slug: string;
  kind: "tunnel" | "direct";
  isAdmin: boolean;
}

// A synced list can hold up to 5000 models; rows are added a page at a time.
const PAGE = 100;

function fmtInt(n: number): string {
  return n.toLocaleString("en-US");
}

function reason(e: unknown, fallback: string): string {
  if (!(e instanceof ApiError)) return fallback;
  if (e.status === 403) return "You don't have permission to change this provider's models.";
  return e.message;
}

/**
 * The provider's stored model list, which GET /ai/<provider>/v1/models
 * answers from. Admins sync it from a direct provider's upstream and add or
 * remove single ids; everyone else reads it.
 */
export function ProviderModelsPanel({ slug, kind, isAdmin }: ProviderModelsPanelProps) {
  const qc = useQueryClient();
  const id = useId();
  const rootRef = useRef<HTMLElement>(null);
  // Same key as the provider page, which reads the first model for its examples.
  const models = useQuery({
    queryKey: ["ai", "provider-models", slug],
    queryFn: () => apiFetch<AiProviderModel[]>(`/ai/providers/${slug}/models`),
    retry: false,
  });
  const [newId, setNewId] = useState("");
  const [filter, setFilter] = useState("");
  const [shown, setShown] = useState(PAGE);
  const [synced, setSynced] = useState<number | null>(null);
  // actionErr: a failed sync or removal; addErr belongs to the "Model id" input.
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [addErr, setAddErr] = useState<string | null>(null);

  // The list, and the model count shown on the provider and in the list of providers.
  const refresh = () => Promise.all([
    qc.invalidateQueries({ queryKey: ["ai", "provider-models", slug] }),
    qc.invalidateQueries({ queryKey: ["ai", "provider", slug], exact: true }),
    qc.invalidateQueries({ queryKey: ["ai", "providers"] }),
  ]);

  const sync = useMutation({
    mutationFn: () => apiFetch<{ count: number }>(`/ai/providers/${slug}/models/sync`, { method: "POST" }),
    onMutate: () => { setSynced(null); setActionErr(null); },
    onSuccess: async (r) => {
      await refresh();
      setSynced(r.count);
    },
    onError: (e: unknown) => setActionErr(reason(e, "Couldn't sync the models.")),
  });

  const add = useMutation({
    mutationFn: (modelId: string) =>
      apiFetch<void>(`/ai/providers/${slug}/models`, { method: "POST", body: JSON.stringify({ id: modelId }) }),
    onMutate: () => { setSynced(null); setAddErr(null); },
    onSuccess: async () => {
      setNewId("");
      await refresh();
    },
    onError: (e: unknown) => setAddErr(reason(e, "Couldn't add the model.")),
  });

  const remove = useMutation({
    // A query parameter because model ids contain "/".
    mutationFn: (modelId: string) =>
      apiFetch<void>(`/ai/providers/${slug}/models?id=${encodeURIComponent(modelId)}`, { method: "DELETE" }),
    onMutate: () => { setSynced(null); setActionErr(null); },
    onSuccess: async () => {
      await refresh();
      // The row and its button are gone; keep the focus in the panel.
      const at = document.activeElement;
      if (!at || at === document.body || (at instanceof HTMLButtonElement && at.disabled)) rootRef.current?.focus();
    },
    onError: (e: unknown, modelId) => setActionErr(`Couldn't remove ${modelId}: ${reason(e, "the request failed")}`),
  });

  function onAdd(e: FormEvent) {
    e.preventDefault();
    const v = newId.trim();
    if (v !== "" && !add.isPending) add.mutate(v);
  }

  const all = models.data ?? [];
  const q = filter.trim().toLowerCase();
  const matching = q === ""
    ? all
    : all.filter((m) => m.id.toLowerCase().includes(q) || m.display_name.toLowerCase().includes(q));
  const visible = matching.slice(0, shown);
  const rest = matching.length - visible.length;
  const addErrId = `${id}-add-err`;

  return (
    <section ref={rootRef} tabIndex={-1} className="col gap-3" aria-label="Models" aria-busy={sync.isPending}>
      {isAdmin && (
        <form className="row row-end gap-2" style={{ flexWrap: "wrap" }} onSubmit={onAdd} noValidate>
          <div className="form-field field-w-md">
            <label htmlFor={`${id}-add`}>Model id</label>
            <Input
              id={`${id}-add`}
              mono
              value={newId}
              maxLength={200}
              invalid={!!addErr}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              aria-describedby={addErr ? addErrId : undefined}
              onChange={(e) => { setNewId(e.target.value); setAddErr(null); }}
            />
            {addErr && <span id={addErrId} className="error" role="alert">{addErr}</span>}
          </div>
          <Button type="submit" variant="secondary" size="sm" disabled={newId.trim() === "" || add.isPending}>
            Add model
          </Button>
          {kind === "direct" && (
            <Button
              type="button"
              variant="primary"
              size="sm"
              className="ml-auto"
              disabled={sync.isPending}
              onClick={() => sync.mutate()}
            >
              {sync.isPending ? "Syncing…" : "Sync models"}
            </Button>
          )}
        </form>
      )}
      {/* Announces the start of a sync; its result is the status line below. */}
      <span className="visually-hidden" aria-live="polite">{sync.isPending ? "Syncing models…" : ""}</span>
      {synced !== null && <p role="status">{fmtInt(synced)} models synced</p>}
      {actionErr && <p role="alert" className="notice-inline error">{actionErr}</p>}

      {models.error ? (
        <ErrorNotice
          action={<Button variant="secondary" size="sm" onClick={() => void models.refetch()}>Retry</Button>}
        >
          Couldn't load the models: {models.error instanceof ApiError ? models.error.message : "Unknown error"}
        </ErrorNotice>
      ) : models.isLoading ? (
        <div className="table-wrap skel-pad"><SkeletonRows n={3} /></div>
      ) : all.length === 0 ? (
        <EmptyState title="No models yet">
          {!isAdmin
            ? "An administrator can add them."
            : kind === "direct"
              ? "Sync them from the provider or add one by id."
              : "Add one by id. While the list is empty, the model list comes from the service itself."}
        </EmptyState>
      ) : (
        <>
          <div className="form-field field-w-md">
            <label htmlFor={`${id}-filter`}>Filter models</label>
            <Input
              id={`${id}-filter`}
              type="search"
              value={filter}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => { setFilter(e.target.value); setShown(PAGE); }}
            />
          </div>
          <div className="table-wrap">
            <table className="data" aria-label="Models">
              <thead>
                <tr>
                  <th>Model id</th>
                  <th>Name</th>
                  <th>Context</th>
                  {isAdmin && <th className="col-actions" aria-label="Actions"></th>}
                </tr>
              </thead>
              <tbody>
                {visible.map((m) => (
                  <tr key={m.id}>
                    <td><span className="mono" style={{ overflowWrap: "anywhere" }}>{m.id}</span></td>
                    <td style={{ overflowWrap: "anywhere" }}>{m.display_name || <span className="muted">—</span>}</td>
                    <td className="mono">{m.context_length > 0 ? fmtInt(m.context_length) : "—"}</td>
                    {isAdmin && (
                      <td className="col-actions">
                        <button
                          type="button"
                          className="icon-btn"
                          aria-label={`Remove ${m.id}`}
                          // One removal at a time; the others wait, visibly.
                          disabled={remove.isPending}
                          onClick={() => remove.mutate(m.id)}
                        >
                          <Trash2 size={14} aria-hidden="true" />
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
            {matching.length === 0 && <p className="muted small pagination-row">No model matches the filter.</p>}
            {matching.length > 0 && (
              <div className="row row-center gap-2 pagination-row">
                <span className="muted small">
                  {q === ""
                    ? `Showing ${fmtInt(visible.length)} of ${fmtInt(all.length)} models`
                    : `Showing ${fmtInt(visible.length)} of ${fmtInt(matching.length)} matching models (${fmtInt(all.length)} in total)`}
                </span>
                {rest > 0 && (
                  <Button variant="secondary" size="sm" className="ml-auto" onClick={() => setShown((n) => n + PAGE)}>
                    Show {fmtInt(Math.min(PAGE, rest))} more
                  </Button>
                )}
              </div>
            )}
          </div>
        </>
      )}
    </section>
  );
}
