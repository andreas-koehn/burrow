import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, EmptyState, ErrorNotice, NotAuthorized, Segmented, SkeletonRows } from "@/components/ds";
import { fmtCount, fmtUsd, shortId } from "@/lib/costFormat";
import type { AiGatewayKey, AiProvider, CostGroupBy, CostSummary } from "@/lib/contract";

const GROUPINGS: { value: CostGroupBy; label: string; noun: string; unattributed: string }[] = [
  { value: "model", label: "Model", noun: "model", unattributed: "not routed by model" },
  { value: "gateway_key", label: "Key", noun: "key", unattributed: "no gateway key" },
  { value: "provider", label: "Provider", noun: "provider", unattributed: "no provider" },
  { value: "target_model", label: "Target", noun: "target", unattributed: "no target" },
  { value: "dialect", label: "Format", noun: "format", unattributed: "no format" },
];

export default function UsageBreakdown({ window }: { window: CostSummary["window"] }) {
  const [groupBy, setGroupBy] = useState<CostGroupBy>("model");
  const captionId = useId();
  const grouping = GROUPINGS.find((g) => g.value === groupBy)!;

  const summary = useQuery({
    queryKey: ["cost", "groups", window, groupBy],
    queryFn: () => apiFetch<CostSummary>(`/cost/summary?window=${window}&group_by=${groupBy}`),
    retry: false,
    staleTime: 60_000,
  });
  const keys = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
    enabled: groupBy === "gateway_key",
  });
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
    enabled: groupBy === "provider" || groupBy === "target_model",
  });

  const keyName = new Map((keys.data ?? []).map((k) => [k.id, k.name]));
  const flat = new Set((providers.data ?? []).filter((p) => p.billing === "flat").map((p) => p.slug));

  function nameOf(key: string): string {
    if (key === "") return grouping.unattributed;
    if (groupBy === "gateway_key") return keyName.get(key) ?? `key ${shortId(key)}`;
    return key;
  }
  function isFlat(key: string): boolean {
    if (groupBy === "provider") return flat.has(key);
    if (groupBy === "target_model") return flat.has(key.split("/")[0] ?? "");
    return false;
  }

  const forbidden = summary.error instanceof ApiError && summary.error.status === 403;
  const rows = summary.data?.groups ?? [];

  let body;
  if (forbidden) {
    body = (
      <NotAuthorized title="You can't view cost data">
        Cost by {grouping.noun} needs the quotas:read:any permission. Ask an admin for access.
      </NotAuthorized>
    );
  } else if (summary.isError) {
    body = (
      <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void summary.refetch()}>Retry</Button>}>
        {summary.error instanceof Error ? summary.error.message : "Couldn't load usage."}
      </ErrorNotice>
    );
  } else if (summary.isLoading || !summary.data || (groupBy === "gateway_key" && keys.isLoading)) {
    body = <SkeletonRows n={3} />;
  } else if (rows.length === 0) {
    body = <EmptyState title="No usage in this period">Requests through the AI gateway show up here as they are metered.</EmptyState>;
  } else {
    body = (
      <>
      <div className="table-wrap">
        <table className="data" aria-label={`Usage by ${grouping.noun}`} aria-describedby={captionId}>
          <thead>
            <tr>
              <th scope="col">{grouping.label}</th>
              <th scope="col" className="col-num">Requests</th>
              <th scope="col" className="col-num">Tokens in</th>
              <th scope="col" className="col-num">Tokens out</th>
              <th scope="col" className="col-num">Cost</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key === "" ? "(none)" : r.key}>
                <td className={r.key === "" ? undefined : "mono"}>{nameOf(r.key)}</td>
                <td className="mono col-num">{fmtCount(r.requests)}</td>
                <td className="mono col-num">{fmtCount(r.tokens_in)}</td>
                <td className="mono col-num">{fmtCount(r.tokens_out)}</td>
                <td className="mono col-num">{isFlat(r.key) ? <Badge nodot>flat rate</Badge> : fmtUsd(r.usd)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p id={captionId} className="muted small">
        Largest first. The row for requests with no {grouping.noun} also holds the remainder beyond the 200 largest groups.
      </p>
      </>
    );
  }

  return (
    <section className="card" aria-labelledby={`${captionId}-h`}>
      <h2 id={`${captionId}-h`}>Usage</h2>
      <div className="toolbar-row">
        <Segmented aria-label="Group usage by" options={GROUPINGS} value={groupBy} onChange={setGroupBy} />
      </div>
      {body}
    </section>
  );
}
