import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import {
  Badge, Button, Dialog, ErrorNotice, Input, PageHeader, Select, SkeletonRows, Tabs,
} from "@/components/ds";
import type { InspectorEntry, Service, ServiceAIConfig } from "@/lib/contract";
import { withAIConfigDefaults } from "@/lib/aiConfig";

function RedactedHeaders({ headers }: { headers: Record<string, string> }) {
  const entries = Object.entries(headers);
  return (
    <table className="data" aria-label="Headers">
      <thead><tr><th>Name</th><th>Value</th></tr></thead>
      <tbody>
        {entries.map(([k, v]) => (
          <tr key={k}>
            <td className="mono">{k}</td>
            <td className="mono small">{v}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
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
        onChange={(id) => { setSelected(null); setQuery(""); nav(`/inspector/${id}`); }}
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
        <p className="muted">
          Request inspector is off for this service — enable in Access settings.
        </p>
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
              <Link to="/inspector">Back to request inspector</Link>
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
              <tr><th>When</th><th>Method</th><th>Path</th><th>Status</th><th>Cache</th></tr>
            </thead>
            <tbody>
              {rows.length === 0
                ? <tr><td colSpan={5} className="muted">No requests yet.</td></tr>
                : rows.map((r) => (
                    <tr
                      key={r.id}
                      onClick={() => { setSelected(r.id); nav(`/inspector/${serviceId}/${r.id}`); }}
                      className="clickable"
                      aria-selected={selected === r.id}
                    >
                      <td className="mono small">{r.ts}</td>
                      <td className="mono">{r.method}</td>
                      <td className="mono">{r.path}</td>
                      <td><Badge nodot kind={`status-${Math.floor(r.status / 100)}xx`}>{r.status}</Badge></td>
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
              <Tabs
                value={tab}
                onChange={setTab}
                tabs={[
                  { value: "request", label: "Request", content: (
                    <>
                      <h3>Headers</h3>
                      <RedactedHeaders headers={detail.data.req_headers} />
                      <h3>Body</h3>
                      <pre className="mono small">{detail.data.req_body}</pre>
                    </>
                  ) },
                  { value: "response", label: "Response", content: (
                    <>
                      <h3>Headers</h3>
                      <RedactedHeaders headers={detail.data.resp_headers} />
                      <h3>Body</h3>
                      <pre className="mono small">{detail.data.resp_body}</pre>
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
        <p>This replays the captured request through the same endpoint.</p>
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
