import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { SearchX } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, EmptyState, ErrorNotice, FormField, Input, SkeletonRows } from "@/components/ds";
import type { AiRequestAttempt } from "@/lib/contract";

/** What an attempt came to, in words. The code is the relay's (internal/aigateway). */
const RESULT: Record<string, string> = {
  timeout: "timed out",
  no_response: "no response",
  busy: "no free place",
  breaker_open: "skipped: provider failing",
  dialect_mismatch: "skipped: other API format",
  client_closed: "client left",
  stream_aborted: "stream broke off",
  panic: "internal error",
};

function resultText(a: AiRequestAttempt): string {
  if (a.error_code === "" && a.status < 400) return "answered";
  return RESULT[a.error_code] ?? (a.status > 0 ? `HTTP ${a.status}` : a.error_code);
}

function durationText(ms: number): string {
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

/**
 * Looks up what one gateway request tried: each target in the order it was
 * tried, what came of it and how long it took. For admins; the relay refuses
 * everyone else.
 */
export function AttemptLookup() {
  const titleId = useId();
  const [text, setText] = useState("");
  // The id that was looked up; "" before the first lookup.
  const [id, setId] = useState("");
  const attempts = useQuery({
    queryKey: ["ai", "request-attempts", id],
    // The relay's ids hold a "/".
    queryFn: () => apiFetch<AiRequestAttempt[]>(`/ai/requests/${encodeURIComponent(id)}/attempts`),
    retry: false,
    enabled: id !== "",
  });
  const rows = Array.isArray(attempts.data) ? attempts.data : [];

  return (
    <section className="card attempt-lookup" aria-labelledby={titleId}>
      <h2 id={titleId} className="card-title">Why did a request fall back?</h2>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const next = text.trim();
          if (next === "") return;
          // The same id again: ask again.
          if (next === id) void attempts.refetch();
          setId(next);
        }}
      >
        <FormField
          label="Request id"
          htmlFor="attempt-request-id"
          w="md"
          help={<span id="attempt-request-id-help">The id is in the Burrow-Request-Id response header.</span>}
        >
          <Input
            id="attempt-request-id"
            mono
            value={text}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            aria-describedby="attempt-request-id-help"
            onChange={(e) => setText(e.target.value)}
          />
        </FormField>
        <Button type="submit" variant="secondary" disabled={text.trim() === "" || attempts.isFetching}>Look up</Button>
      </form>

      {id === "" ? null : attempts.isFetching ? (
        <div className="table-wrap skel-pad" aria-busy="true">
          <SkeletonRows n={2} />
        </div>
      ) : attempts.error ? (
        attempts.error instanceof ApiError && attempts.error.status === 403 ? (
          <ErrorNotice>Only an administrator can read attempt logs.</ErrorNotice>
        ) : (
          <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void attempts.refetch()}>Retry</Button>}>
            Couldn't load the attempts:{" "}
            {attempts.error instanceof ApiError ? attempts.error.message : "Unknown error"}
          </ErrorNotice>
        )
      ) : rows.length === 0 ? (
        <EmptyState icon={<SearchX size={18} />} title="No attempt log for this request">
          A log is only kept for requests that needed more than one attempt or failed. A request its first
          target answered leaves none; neither does an id the relay never gave out.
        </EmptyState>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label={`Attempts of request ${id}`}>
            <thead>
              <tr>
                <th>Order</th>
                <th>Target</th>
                <th>Result</th>
                <th>Duration</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((a, i) => (
                <tr key={a.position}>
                  <td>{i + 1}</td>
                  <td><span className="mono">{`${a.provider}/${a.model}`}</span></td>
                  <td>{resultText(a)}</td>
                  <td>{durationText(a.duration_ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
