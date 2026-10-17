import { useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ApiError, apiFetch } from "@/lib/api";
import { Button, ErrorNotice, Input, SkeletonRows, TableEmptyRow } from "@/components/ds";
import type { PricingEntry, PricingTable } from "@/lib/contract";

type Field = "input_per_million" | "output_per_million" | "per_request_usd";
const COLUMNS: { field: Field; header: string; label: string }[] = [
  { field: "input_per_million", header: "Input (USD per million)", label: "Input (USD per million)" },
  { field: "output_per_million", header: "Output (USD per million)", label: "Output (USD per million)" },
  { field: "per_request_usd", header: "Per request (USD)", label: "Per request (USD)" },
];

const rowKey = (e: PricingEntry) => `${e.provider}/${e.model}`;

/** An empty field is 0; anything else must be a number of at least 0. */
function parsePrice(text: string): number | null {
  if (text.trim() === "") return 0;
  const n = Number(text);
  return Number.isFinite(n) && n >= 0 ? n : null;
}

/**
 * The price table in use, one row per provider and model. Tokens are priced by the first two
 * columns; a request that reports no tokens (images, speech, transcription) by the third.
 * Saving sends the whole table, as the relay replaces it.
 */
export function PricingEditor() {
  const qc = useQueryClient();
  const help = useId();
  const table = useQuery({
    queryKey: ["cost", "pricing"],
    queryFn: () => apiFetch<PricingTable>("/cost/pricing"),
    retry: false,
  });
  // What the operator typed, by row and field; a field left alone shows the stored price.
  const [typed, setTyped] = useState<Record<string, Partial<Record<Field, string>>>>({});
  const [left, setLeft] = useState<Record<string, boolean>>({});
  const [failure, setFailure] = useState<string | null>(null);

  const entries = table.data?.entries ?? [];
  const textOf = (e: PricingEntry, f: Field) => typed[rowKey(e)]?.[f] ?? String(e[f] ?? 0);
  const invalid = (e: PricingEntry, f: Field) => parsePrice(textOf(e, f)) === null;
  const anyInvalid = entries.some((e) => COLUMNS.some((c) => invalid(e, c.field)));

  const save = useMutation({
    mutationFn: () => {
      const body: PricingTable = {
        version: table.data!.version,
        entries: entries.map((e) => ({
          provider: e.provider,
          model: e.model,
          input_per_million: parsePrice(textOf(e, "input_per_million"))!,
          output_per_million: parsePrice(textOf(e, "output_per_million"))!,
          per_request_usd: parsePrice(textOf(e, "per_request_usd"))!,
        })),
      };
      return apiFetch<void>("/cost/pricing", { method: "PUT", body: JSON.stringify(body) });
    },
    onSuccess: () => {
      setFailure(null);
      setTyped({});
      setLeft({});
      toast.success("Prices saved.");
      void qc.invalidateQueries({ queryKey: ["cost"] });
    },
    onError: (e) => setFailure(e instanceof ApiError ? e.message : "Couldn't save the prices."),
  });

  return (
    <section className="card" aria-labelledby={`${help}-title`}>
      <h2 id={`${help}-title`}>Prices</h2>
      <p className="muted small" id={help}>
        A request with token counts is priced by tokens. A request without them (images, speech, transcription, rerank)
        is priced by its per-request price; empty means 0.
      </p>
      {table.isLoading ? (
        <SkeletonRows n={3} />
      ) : table.isError ? (
        <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void table.refetch()}>Retry</Button>}>
          {table.error instanceof Error ? table.error.message : "Couldn't load the prices."}
        </ErrorNotice>
      ) : (
        <>
          <div className="table-wrap">
            <table className="data" aria-label="Prices" aria-describedby={help}>
              <thead>
                <tr>
                  <th scope="col">Provider</th>
                  <th scope="col">Model</th>
                  {COLUMNS.map((c) => <th key={c.field} scope="col" className="col-num">{c.header}</th>)}
                </tr>
              </thead>
              <tbody>
                {entries.length === 0
                  ? <TableEmptyRow colSpan={5} title="No prices yet." />
                  : entries.map((e) => (
                      <tr key={rowKey(e)}>
                        <td className="mono">{e.provider}</td>
                        <td className="mono">{e.model}</td>
                        {COLUMNS.map((c) => {
                          const id = `price-${rowKey(e)}-${c.field}`;
                          const bad = left[id] && invalid(e, c.field);
                          return (
                            <td key={c.field} className="col-num">
                              <Input
                                type="number"
                                min={0}
                                step="any"
                                mono
                                aria-label={`${c.label} for ${rowKey(e)}`}
                                invalid={bad || undefined}
                                aria-describedby={bad ? `${id}-err` : undefined}
                                value={textOf(e, c.field)}
                                onChange={(ev) => setTyped((t) => ({ ...t, [rowKey(e)]: { ...t[rowKey(e)], [c.field]: ev.target.value } }))}
                                onBlur={() => setLeft((l) => ({ ...l, [id]: true }))}
                              />
                              {bad && <div id={`${id}-err`} className="field-error small">Enter 0 or more.</div>}
                            </td>
                          );
                        })}
                      </tr>
                    ))}
              </tbody>
            </table>
          </div>
          {failure && <ErrorNotice>{failure}</ErrorNotice>}
          <div className="toolbar-row">
            <Button variant="primary" size="sm" disabled={save.isPending || anyInvalid || entries.length === 0} onClick={() => save.mutate()}>
              Save prices
            </Button>
          </div>
        </>
      )}
    </section>
  );
}
