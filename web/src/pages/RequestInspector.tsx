import { useEffect, useState, type MouseEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import {
  Badge, Button, Dialog, ErrorNotice, Input, PageHeader, Select, SkeletonRows, Tabs, TableEmptyRow,
} from "@/components/ds";
import type { InspectorEntry, Service, ServiceAIConfig } from "@/lib/contract";
import { withAIConfigDefaults } from "@/lib/aiConfig";
import { InspectorOffHint } from "@/components/InspectorOffHint";
import { RelativeTime } from "@/components/RelativeTime";
import { droppedNames, pairWords } from "@/lib/translation";
import { binaryBodyNote, guardrailMarks } from "@/lib/guardrailMarks";
import { isToolCall, toolArguments, toolOf, toolResult } from "@/lib/mcpCall";

function RedactedHeaders({ headers }: { headers: Record<string, string> }) {
  const entries = Object.entries(headers);
  return (
    <table className="data" aria-label="Headers">
      <thead><tr><th>Name</th><th>Value</th></tr></thead>
      <tbody>
        {entries.map(([k, v]) => (
          <tr key={k}>
            <td className="mono">{k}</td>
            <td className="mono">{v}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/**
 * Says that a request was translated, in which direction, and what the
 * translation left out. A name can end in text the client chose (after a
 * prefix such as "unknown:"), so every name is a text node: never markup,
 * never a link.
 */
function TranslationNote({ entry }: { entry: InspectorEntry }) {
  if (!entry.translated) return null;
  const { names, more } = droppedNames(entry.dropped);
  return (
    <div className="translation-note" role="group" aria-label="Translation">
      <dl className="def-list">
        <div className="def-row">
          <dt className="def-key">Translated</dt>
          <dd className="def-val">{pairWords(entry.translated)}</dd>
        </div>
        {(names.length > 0 || more) && (
          <div className="def-row">
            <dt className="def-key">Left out</dt>
            <dd className="def-val">
              {names.length > 0 && (
                <ul className="dropped-list" aria-label="Left out">
                  {names.map((n) => <li key={n} className="mono small">{n}</li>)}
                </ul>
              )}
              {more && <span className="muted small">and more</span>}
            </dd>
          </div>
        )}
      </dl>
    </div>
  );
}

/** What the guardrails did to a request: the pattern, the judge's verdict and its reason in full. */
function GuardrailNote({ entry }: { entry: InspectorEntry }) {
  const marks = guardrailMarks(entry);
  if (marks.length === 0) return null;
  const label = { guardrail: "Guardrail", judge: "Judge", "judge-call": "Purpose" } as const;
  return (
    <div className="translation-note" role="group" aria-label="Guardrail">
      <dl className="def-list">
        {marks.map((m) => (
          <div key={m.kind} className="def-row">
            <dt className="def-key">{label[m.kind]}</dt>
            <dd className="def-val">
              <span>{m.text}</span>
              {m.direction && <> <span className="muted small">{m.direction === "answer" ? "Answer" : "Request"}</span></>}
              {m.detail && <p className="small">{m.detail}</p>}
              {m.reason && <p className="small">{m.reason}</p>}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

/**
 * A tools/call served by the tool gateway: the source, the tool, its arguments and
 * its result, as the inspector stored them (the redaction rules have already run).
 */
function McpCallNote({ entry }: { entry: InspectorEntry }) {
  if (!isToolCall(entry)) return null;
  const args = toolArguments(entry.mcp?.params);
  const result = toolResult(entry.resp_body ?? "");
  return (
    <div className="translation-note" role="group" aria-label="MCP call">
      <dl className="def-list">
        <div className="def-row"><dt className="def-key">Method</dt><dd className="def-val mono">{entry.mcp?.method}</dd></div>
        {entry.provider && <div className="def-row"><dt className="def-key">Source</dt><dd className="def-val">{entry.provider}</dd></div>}
        <div className="def-row"><dt className="def-key">Tool</dt><dd className="def-val mono">{toolOf(entry)}</dd></div>
      </dl>
      {args && <><h3>Arguments</h3><BodyView body={args} label="Arguments" /></>}
      {result.text && (
        <>
          <h3>Result</h3>
          {result.isError && <p className="small">The tool reported an error.</p>}
          <BodyView body={result.text} label="Result" />
        </>
      )}
    </div>
  );
}

/** A body in the inspector: text as stored, or a sentence for a binary body the relay did not keep. */
function BodyView({ body, label }: { body: string; label: string }) {
  const note = binaryBodyNote(body);
  // Focusable, so a body wider than the panel can be scrolled by keyboard.
  return note ? <p className="muted">{note}</p> : <pre className="mono small" tabIndex={0} role="region" aria-label={label}>{body}</pre>;
}

export default function RequestInspector() {
  const { serviceId = "", requestId } = useParams<{ serviceId: string; requestId?: string }>();
  const nav = useNavigate();
  const qc = useQueryClient();
  const [selected, setSelected] = useState<string | null>(requestId ?? null);
  const [query, setQuery] = useState("");
  const [replayOpen, setReplayOpen] = useState(false);
  const [compareOpen, setCompareOpen] = useState(false);
  const [tab, setTab] = useState("request");

  useEffect(() => {
    if (requestId) setSelected(requestId);
  }, [requestId]);

  // A request opens on a click anywhere in its row, as a service does on the Services list.
  function openRow(e: MouseEvent<HTMLTableRowElement>, id: string) {
    if ((e.target as Element).closest("a")) return;
    // A click with a key held asks for a new tab or window, which only the path's link can open.
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    // Marking a path to copy it ends in a click too.
    if (window.getSelection()?.toString()) return;
    setSelected(id);
    nav(`/gateway/requests/${serviceId}/${id}`);
  }

  const cfg = useQuery({
    queryKey: ["service", serviceId, "ai-config"],
    queryFn: () =>
      apiFetch<Partial<ServiceAIConfig>>(`/services/${serviceId}/ai-config`).then(withAIConfigDefaults),
    retry: false,
    enabled: Boolean(serviceId),
  });
  const list = useQuery({
    queryKey: ["inspector", serviceId],
    queryFn: () => apiFetch<InspectorEntry[]>(`/services/${serviceId}/inspector/requests?limit=100`),
    retry: false,
    enabled: Boolean(serviceId),
  });
  const detail = useQuery({
    queryKey: ["inspector", serviceId, selected],
    queryFn: () => apiFetch<InspectorEntry>(`/services/${serviceId}/inspector/requests/${selected}`),
    retry: false,
    enabled: Boolean(serviceId && selected),
  });
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const httpServices = (Array.isArray(services.data) ? services.data : []).filter((s) => s.type === "http");
  const servicePicker = httpServices.length > 1 ? (
    <div className="row row-center gap-2">
      <label htmlFor="inspector-service" className="muted small">Service</label>
      <Select
        id="inspector-service"
        options={httpServices.map((s) => ({ value: s.id, label: s.name }))}
        value={serviceId}
        onChange={(id) => { setSelected(null); setQuery(""); nav(`/gateway/requests/${id}`); }}
      />
    </div>
  ) : undefined;
  const currentName = httpServices.find((s) => s.id === serviceId)?.name;

  useEffect(() => {
    if (typeof EventSource === "undefined") return;
    const es = new EventSource("/api/v1/events");
    const onReq = () => qc.invalidateQueries({ queryKey: ["inspector", serviceId] });
    es.addEventListener("request", onReq);
    return () => { es.removeEventListener("request", onReq); es.close(); };
  }, [qc, serviceId]);

  const replay = useMutation({
    mutationFn: () =>
      apiFetch<{ new_entry: InspectorEntry }>(
        `/services/${serviceId}/inspector/requests/${selected}/replay`,
        { method: "POST", body: "{}" },
      ),
    onSuccess: (res) => {
      toast.success("Request replayed.");
      qc.invalidateQueries({ queryKey: ["inspector", serviceId] });
      setReplayOpen(false);
      setSelected(res.new_entry.id);
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't replay request."),
  });

  // The server returns the structured diff shape
  // {original, replayed, diff:{headers:string[], body:string}} — see
  // internal/api/inspector_handlers.go compareDiff. `diff` is an OBJECT, not a
  // string: rendering it directly in JSX throws React error #31 and blanks the
  // page, so we keep the typed shape and render its parts below.
  const compare = useMutation({
    mutationFn: () =>
      apiFetch<{ diff: { headers: string[]; body: string } }>(
        `/services/${serviceId}/inspector/requests/${selected}/replay-compare`,
        { method: "POST", body: "{}" },
      ),
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't run replay-compare."),
  });

  if (cfg.data && !cfg.data.inspector.enabled) {
    return (
      <div className="inspector-page">
        <PageHeader title="Request inspector" actions={servicePicker} />
        <p className="muted">Request inspector is off for this service.</p>
        <p className="muted small"><InspectorOffHint serviceId={serviceId} /></p>
      </div>
    );
  }

  // Unknown service id or a failed first load: an error, not an endless
  // skeleton. A failed background refetch keeps the list already on screen.
  if (list.isError && !list.data) {
    return (
      <div className="inspector-page">
        <PageHeader title="Request inspector" actions={servicePicker} />
        <ErrorNotice
          action={
            <>
              <Button variant="secondary" size="sm" onClick={() => void list.refetch()}>
                Retry
              </Button>
              <Link to="/gateway/requests">Back to request inspector</Link>
            </>
          }
        >
          Couldn't load requests:{" "}
          {list.error instanceof ApiError ? list.error.message : "Unknown error"}
        </ErrorNotice>
      </div>
    );
  }

  if (!list.data) {
    return (
      <div className="inspector-page">
        <PageHeader title="Request inspector" actions={servicePicker} />
        <SkeletonRows n={6} />
      </div>
    );
  }

  const rows = list.data.filter((r) =>
    `${r.path} ${r.method} ${r.req_body}`.toLowerCase().includes(query.toLowerCase()),
  );

  return (
    <div className="inspector-page">
      <PageHeader
        title="Request inspector"
        subtitle={currentName ? `Tail and replay traffic on ${currentName}.` : "Tail and replay traffic on this service."}
        actions={servicePicker}
      />

      <div className="inspector-search">
        <Input
          type="search"
          role="searchbox"
          aria-label="Search requests"
          placeholder="search by path / method / body"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
      </div>

      <div className="inspector-grid">
        <div className="table-wrap">
          <table className="data" aria-label="Requests">
            <thead>
              <tr>
                <th scope="col">When</th><th scope="col">Method</th><th scope="col">Path</th>
                <th scope="col">Status</th><th scope="col">Cache</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0
                ? (list.data.length === 0
                  ? <TableEmptyRow colSpan={5} title="No requests yet.">Requests appear here as soon as traffic reaches this service.</TableEmptyRow>
                  : <TableEmptyRow colSpan={5} title="No requests match your search." />)
                : rows.map((r) => (
                    // The path is the link, and the one stop for the keyboard: the row adds a bigger target only.
                    <tr key={r.id} className="clickable" onClick={(e) => openRow(e, r.id)}>
                      <td><RelativeTime value={r.ts} /></td>
                      <td className="mono">{r.method}</td>
                      <td>
                        <Link
                          className="mono"
                          to={`/gateway/requests/${serviceId}/${r.id}`}
                          aria-current={selected === r.id ? "true" : undefined}
                        >
                          {isToolCall(r) ? `tools/call · ${toolOf(r)}` : r.path}
                        </Link>
                      </td>
                      <td>
                        <Badge nodot kind={`status-${Math.floor(r.status / 100)}xx`}>{r.status}</Badge>
                        {r.translated && <>{" "}<Badge nodot kind="status-idle">translated</Badge></>}
                      </td>
                      <td className="mono">{r.cache}</td>
                    </tr>
                  ))}
            </tbody>
          </table>
        </div>

        <div className="detail-pane">
          {!detail.data ? (
            <p className="muted">Select a request to inspect.</p>
          ) : (
            <>
              <div className="detail-toolbar">
                <Button
                  variant="secondary"
                  size="sm"
                  aria-label="Open replay dialog"
                  onClick={() => setReplayOpen(true)}
                >
                  Replay
                </Button>
                <Button variant="secondary" size="sm" onClick={() => { compare.mutate(); setCompareOpen(true); }}>
                  Replay &amp; compare
                </Button>
              </div>
              <GuardrailNote entry={detail.data} />
              <TranslationNote entry={detail.data} />
              <McpCallNote entry={detail.data} />
              <Tabs
                value={tab}
                onChange={setTab}
                tabs={[
                  { value: "request", label: "Request", content: (
                    <>
                      <h3>Headers</h3>
                      <RedactedHeaders headers={detail.data.req_headers} />
                      <h3>Body</h3>
                      <BodyView body={detail.data.req_body} label="Request body" />
                    </>
                  ) },
                  { value: "response", label: "Response", content: (
                    <>
                      <h3>Headers</h3>
                      <RedactedHeaders headers={detail.data.resp_headers} />
                      <h3>Body</h3>
                      <BodyView body={detail.data.resp_body} label="Response body" />
                    </>
                  ) },
                  { value: "timing", label: "Timing", content: (
                    <p className="mono">duration: {detail.data.duration_ms} ms</p>
                  ) },
                  { value: "trace", label: "Trace", content: (
                    <p className="mono">trace_id: {detail.data.trace_id}</p>
                  ) },
                ]}
              />
            </>
          )}
        </div>
      </div>

      <Dialog
        open={replayOpen}
        onOpenChange={setReplayOpen}
        title="Replay request"
        footer={
          <>
            <Button variant="secondary" onClick={() => setReplayOpen(false)}>Cancel</Button>
            <Button variant="primary" disabled={replay.isPending} onClick={() => replay.mutate()}>
              {replay.isPending ? "Replaying…" : "Replay"}
            </Button>
          </>
        }
      >
        <p>This sends the captured request again.</p>
      </Dialog>

      <Dialog
        open={compareOpen}
        onOpenChange={setCompareOpen}
        title="Replay & compare"
        footer={<Button variant="secondary" onClick={() => setCompareOpen(false)}>Close</Button>}
      >
        {compare.isPending ? (
          <p className="muted">Running replay…</p>
        ) : compare.data ? (
          <>
            <h3>Response header changes</h3>
            {compare.data.diff.headers.length === 0 ? (
              <p className="muted">No header changes.</p>
            ) : (
              <pre className="mono small">{compare.data.diff.headers.join("\n")}</pre>
            )}
            <h3>Body diff</h3>
            <pre className="mono small">{compare.data.diff.body || "(no body diff)"}</pre>
          </>
        ) : (
          <p className="muted">(no diff)</p>
        )}
      </Dialog>
      <Toaster />
    </div>
  );
}
