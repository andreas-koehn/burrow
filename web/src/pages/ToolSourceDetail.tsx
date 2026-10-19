import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil } from "lucide-react";
import { toast } from "sonner";
import { Toaster } from "@/components/ui/sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, PageHeader, SkeletonRows, Switch } from "@/components/ds";
import { CopyableCode } from "@/components/CopyableCode";
import { RelativeTime } from "@/components/RelativeTime";
import { ToolDefinitionView } from "@/components/ToolDefinitionView";
import { ToolOverridesDialog } from "@/components/ToolOverridesDialog";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { sourceStatus, toolStatus } from "@/lib/toolSource";
import type { ToolApproveAllResult, ToolRenameKey, ToolSource, ToolSourceTool, ToolSyncResult } from "@/lib/contract";

const BACK = { to: "/gateway/tools", label: "Tools" } as const;
const noun = (n: number) => (n === 1 ? "1 tool" : `${n} tools`);
const errText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback);

export default function ToolSourceDetail() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const nav = useNavigate();
  const canManage = useCan(P.mcpSourcesManage);
  const base = `/mcp/sources/${encodeURIComponent(slug)}`;

  const source = useQuery({ queryKey: ["mcp", "sources", slug], queryFn: () => apiFetch<ToolSource>(base), retry: false });
  const tools = useQuery({ queryKey: ["mcp", "sources", slug, "tools"], queryFn: () => apiFetch<ToolSourceTool[]>(`${base}/tools`), retry: false });

  // What the last action said: shown until the next one.
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [conflictNote, setConflictNote] = useState<string | null>(null);
  const [renameKeys, setRenameKeys] = useState<{ name: string; keys: ToolRenameKey[] } | null>(null);
  const [editing, setEditing] = useState<ToolSourceTool | null>(null);
  // The pending tools as the approve-all dialog showed them: the hashes sent are exactly these.
  const [confirmAll, setConfirmAll] = useState<ToolSourceTool[] | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);

  const refresh = () => qc.invalidateQueries({ queryKey: ["mcp", "sources"] });
  function clearNotes() { setActionErr(null); setInfo(null); setConflictNote(null); setRenameKeys(null); }

  const sync = useMutation({
    mutationFn: () => apiFetch<ToolSyncResult>(`${base}/sync`, { method: "POST" }),
    onMutate: clearNotes,
    onSuccess: async (r) => {
      setInfo(`Read ${noun(r.tool_count)}: ${r.added} new, ${r.changed} changed, ${r.removed} removed.${r.skipped.length ? ` Skipped: ${r.skipped.join(", ")}.` : ""}`);
      await refresh();
    },
    onError: async (e) => { setActionErr(errText(e, "The tools could not be read.")); await refresh(); },
  });

  const approve = useMutation({
    // The hash is the one the reader saw next to the definition.
    mutationFn: (t: ToolSourceTool) =>
      apiFetch<ToolSourceTool>(`${base}/tools/${encodeURIComponent(t.name)}/approve`, { method: "POST", body: JSON.stringify({ hash: t.current_hash }) }),
    onMutate: clearNotes,
    onSuccess: async (_t, t) => { setInfo(`Approved ${t.name}.`); await refresh(); },
    onError: async (e) => { setActionErr(errText(e, "Couldn't approve the tool.")); await refresh(); },
  });

  const approveAll = useMutation({
    mutationFn: (shown: ToolSourceTool[]) =>
      apiFetch<ToolApproveAllResult>(`${base}/approve-all`, {
        method: "POST",
        body: JSON.stringify({ hashes: Object.fromEntries(shown.map((t) => [t.name, t.current_hash])) }),
      }),
    onMutate: clearNotes,
    onSuccess: async (r, shown) => {
      setConfirmAll(null);
      const conflicts = r.conflicts ?? [];
      const approved = r.approved ?? [];
      setInfo(`Approved ${noun(approved.length)}.`);
      if (conflicts.length > 0) {
        setConflictNote(`Not approved: ${conflicts.join(", ")}. Another tool of this source is already served under the same name. Rename one of them, then approve it.`);
      }
      const left = shown.length - approved.length - conflicts.length;
      if (left > 0) setActionErr(`${noun(left)} changed since you looked and ${left === 1 ? "was" : "were"} not approved. Review ${left === 1 ? "it" : "them"} again.`);
      await refresh();
    },
    onError: async (e) => { setConfirmAll(null); setActionErr(errText(e, "Couldn't approve the tools.")); await refresh(); },
  });

  const remove = useMutation({
    mutationFn: () => apiFetch<void>(base, { method: "DELETE" }),
    onSuccess: async () => {
      nav("/gateway/tools");
      await refresh();
    },
    onError: (e: unknown) => {
      if (e instanceof ApiError && e.status === 403) setDeleteErr("You don't have permission to delete tool sources.");
      else setDeleteErr(errText(e, "Couldn't delete the tool source."));
    },
  });

  if (source.error) {
    const missing = source.error instanceof ApiError && source.error.status === 404;
    return (
      <div className="ai-endpoint-detail-page">
        <PageHeader back={BACK} title="Tool source" />
        <ErrorNotice action={missing ? undefined : <Button variant="secondary" size="sm" onClick={() => void source.refetch()}>Retry</Button>}>
          {missing ? "This tool source was not found. It may have been deleted." : `Couldn't load the tool source: ${errText(source.error, "Unknown error")}`}
        </ErrorNotice>
      </div>
    );
  }
  if (!source.data) {
    return (
      <div className="ai-endpoint-detail-page">
        <PageHeader back={BACK} title="Tool source" />
        <SkeletonRows n={5} />
      </div>
    );
  }
  const src = source.data;
  const list = tools.data ?? [];
  const pending = list.filter((t) => t.status === "pending");
  const conflicts = list.filter((t) => t.serve_issue === "name_conflict");
  const status = sourceStatus(src);

  return (
    <div className="ai-endpoint-detail-page">
      <PageHeader
        back={BACK}
        title={`Tool source · ${src.name}`}
        subtitle={src.kind === "remote" ? "A server on the internet that the relay calls." : "A server on one of your machines, published by a client."}
        actions={canManage ? (
          <>
            <Button variant="secondary" size="sm" disabled={sync.isPending} onClick={() => sync.mutate()}>
              {sync.isPending ? "Reading…" : "Sync now"}
            </Button>
            <Button variant="destructive" size="sm" onClick={() => { setDeleteErr(null); setDeleteOpen(true); }}>Delete source</Button>
          </>
        ) : undefined}
      />

      <div className="col gap-3">
        <section className="card" aria-label="Source">
          <dl className="kv-list">
            <div><dt>Status</dt><dd><Badge kind={status.kind}>{status.label}</Badge></dd></div>
            <div><dt>Address for agents</dt><dd>
              {src.endpoint_url
                ? <CopyableCode text={src.endpoint_url} label={`Copy address ${src.endpoint_url}`} />
                : <span className="mono">/mcp/{src.slug}</span>}
            </dd></div>
            {src.kind === "remote" ? (
              <>
                <div><dt>Server</dt><dd className="mono">{src.url}</dd></div>
                <div><dt>Credential</dt><dd>
                  {src.credential_slot === "" ? "None" : (
                    <>Slot <span className="mono">{src.credential_slot}</span>, {src.credential_present ? "set on the relay" : "not set on the relay yet"}</>
                  )}
                </dd></div>
              </>
            ) : (
              <div><dt>Service</dt><dd className="mono">{src.service_id}</dd></div>
            )}
            <div><dt>Last read</dt><dd><RelativeTime value={src.last_sync_at} empty="Never" /></dd></div>
          </dl>
          {src.last_sync_error && <ErrorNotice>Last sync failed: {src.last_sync_error}</ErrorNotice>}
        </section>

        {actionErr && <ErrorNotice>{actionErr}</ErrorNotice>}
        {info && <ErrorNotice variant="info" role="status">{info}</ErrorNotice>}
        {conflictNote && <ErrorNotice variant="warn" role="status">{conflictNote}</ErrorNotice>}
        {renameKeys && (
          <ErrorNotice variant="warn" role="status" title={`Keys that already name ${src.slug}__${renameKeys.name}`}>
            They now reach this tool:{" "}
            {renameKeys.keys.map((k) => `${k.name} (${k.key_prefix})`).join(", ")}. If that is not meant, rename it again.
          </ErrorNotice>
        )}
        {conflicts.length > 0 && (
          <ErrorNotice variant="warn">
            {conflicts.map((t) => t.name).join(", ")} would be served under the same name, so neither is served. Rename one of them.
          </ErrorNotice>
        )}

        {pending.length > 0 && (
          <section className="card" aria-label="Pending changes">
            <div className="row row-center gap-2" style={{ justifyContent: "space-between" }}>
              <h2 className="card-title" style={{ margin: 0 }}>Pending changes</h2>
              {canManage && (
                <Button variant="secondary" size="sm" onClick={() => setConfirmAll(pending)}>Approve all</Button>
              )}
            </div>
            <p className="muted small">
              These tools are not served until you approve them. The text is the server's own: read it before you approve, an agent will follow it.
            </p>
            <div className="col gap-3">
              {pending.map((t) => (
                <PendingTool key={t.name} tool={t} canManage={canManage} busy={approve.isPending} onApprove={() => approve.mutate(t)} />
              ))}
            </div>
          </section>
        )}

        <section className="card" aria-label="Tools">
          <h2 className="card-title">Tools</h2>
          {tools.error ? (
            <ErrorNotice action={<Button variant="secondary" size="sm" onClick={() => void tools.refetch()}>Retry</Button>}>
              Couldn't load the tools: {errText(tools.error, "Unknown error")}
            </ErrorNotice>
          ) : tools.isLoading ? (
            <SkeletonRows n={3} />
          ) : list.length === 0 ? (
            <p className="muted">No tools yet. {canManage ? "Sync now reads them from the server." : "An admin can read them from the server."}</p>
          ) : (
            <div className="table-wrap">
              <table className="data" aria-label={`Tools of ${src.name}`}>
                <thead>
                  <tr>
                    <th>Tool</th>
                    <th>On /mcp</th>
                    <th>Effect</th>
                    <th>Status</th>
                    {canManage && <th className="col-actions" aria-label="Actions"></th>}
                  </tr>
                </thead>
                <tbody>
                  {list.map((t) => {
                    const st = toolStatus(t);
                    return (
                      <tr key={t.name}>
                        <td className="col-name">
                          <div className="mono">{t.name}</div>
                          {t.served_name !== t.name && <div className="muted small">served as <span className="mono">{t.served_name}</span></div>}
                        </td>
                        <td>
                          {t.aggregated_name ? <span className="mono">{t.aggregated_name}</span> : <span className="muted">—</span>}
                          {t.aggregated_issue ? <div className="muted small">{t.aggregated_issue}</div> : null}
                        </td>
                        <td>
                          {t.effective_read_only ? "Read-only" : "Can change things"}
                          {t.read_only !== null && <div className="muted small">set by you</div>}
                        </td>
                        <td><Badge kind={st.kind}>{st.label}</Badge></td>
                        {canManage && (
                          <td className="col-actions">
                            <button type="button" className="icon-btn" aria-label={`Edit ${t.name}`} onClick={() => setEditing(t)}>
                              <Pencil size={13} aria-hidden="true" />
                            </button>
                          </td>
                        )}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </section>

        <LimitsCard source={src} canManage={canManage} onSaved={refresh} />
      </div>

      {canManage && (
        <ToolOverridesDialog
          slug={src.slug}
          tool={editing}
          onClose={() => setEditing(null)}
          onSaved={(saved, keys) => {
            clearNotes();
            toast.success("Saved.");
            if (keys.length > 0) setRenameKeys({ name: saved.served_name, keys });
          }}
        />
      )}

      {canManage && confirmAll && (
        <Dialog
          open
          onOpenChange={(o) => { if (!o && !approveAll.isPending) setConfirmAll(null); }}
          title={`Approve ${noun(confirmAll.length)}`}
          description="Each tool is approved with the definition shown on this page. A tool the server has changed since is left out."
          footer={
            <>
              <Button variant="secondary" disabled={approveAll.isPending} onClick={() => setConfirmAll(null)}>Cancel</Button>
              <Button variant="primary" disabled={approveAll.isPending} onClick={() => approveAll.mutate(confirmAll)}>
                {approveAll.isPending ? "Approving…" : `Approve ${noun(confirmAll.length)}`}
              </Button>
            </>
          }
        >
          <ul className="col gap-1">
            {confirmAll.map((t) => (
              <li key={t.name}>
                <span className="mono">{t.name}</span> <span className="muted small">{t.approved_hash === "" ? "new" : "changed"}</span>
                {t.guardrail_hits.length > 0 && <span className="muted small"> · matches a guardrail pattern</span>}
              </li>
            ))}
          </ul>
          <p className="muted small">Agents will see and follow these definitions.</p>
        </Dialog>
      )}

      {canManage && (
        <Dialog
          open={deleteOpen}
          onOpenChange={(o) => { if (!remove.isPending) setDeleteOpen(o); }}
          title={`Delete source · ${src.name}`}
          description={`/mcp/${src.slug} stops working at once. This cannot be undone.`}
          footer={
            <>
              <Button variant="secondary" disabled={remove.isPending} onClick={() => setDeleteOpen(false)}>Cancel</Button>
              <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {remove.isPending ? "Deleting…" : "Delete source"}
              </Button>
            </>
          }
        >
          <p className="muted">
            Its tools and approvals are deleted, and gateway keys can no longer use them.{" "}
            {src.kind === "remote"
              ? `Its usage settings and request history go with it. The credential slot ${src.credential_slot || "(none)"} on the relay is not touched.`
              : <>The service <span className="mono">{src.service_id}</span> and its client connection are kept. The service stays gateway-only: it is not reopened automatically. Open it again in the service's access settings if you want direct access.</>}
          </p>
          {deleteErr && <ErrorNotice>{deleteErr}</ErrorNotice>}
        </Dialog>
      )}
      <Toaster />
    </div>
  );
}

function PendingTool({ tool, canManage, busy, onApprove }: { tool: ToolSourceTool; canManage: boolean; busy: boolean; onApprove: () => void }) {
  const fresh = tool.approved_definition === null;
  return (
    <div role="group" aria-label={tool.name} className="col gap-2 pending-tool">
      <div className="row row-center gap-2" style={{ justifyContent: "space-between" }}>
        <div className="row row-center gap-2">
          <span className="mono"><strong>{tool.name}</strong></span>
          <Badge kind="status-idle">{fresh ? "New" : "Changed"}</Badge>
        </div>
        {canManage && <Button variant="primary" size="sm" disabled={busy} onClick={onApprove}>Approve</Button>}
      </div>
      {tool.guardrail_hits.map((h, i) => (
        <ErrorNotice key={i} variant="warn" role="note">
          The {h.field.replace("_", " ")} matches a guardrail pattern: <code>{h.pattern}</code>
        </ErrorNotice>
      ))}
      <div style={{ display: "grid", gridTemplateColumns: fresh ? "1fr" : "repeat(auto-fit, minmax(260px, 1fr))", gap: "var(--space-3, 12px)" }}>
        {!fresh && <ToolDefinitionView label="Approved definition" definition={tool.approved_definition!} other={tool.definition} side="old" />}
        <ToolDefinitionView label={fresh ? "Definition" : "New definition"} definition={tool.definition} other={fresh ? undefined : tool.approved_definition} side="new" />
      </div>
    </div>
  );
}

function wholeNumber(text: string, min: number, max: number): number | null {
  if (text.trim() === "") return null;
  const n = Number(text);
  return Number.isInteger(n) && n >= min && n <= max ? n : null;
}

function LimitsCard({ source, canManage, onSaved }: { source: ToolSource; canManage: boolean; onSaved: () => unknown }) {
  const [rpm, setRpm] = useState(String(source.rpm));
  const [conc, setConc] = useState(String(source.max_concurrent));
  const [timeout, setTimeoutS] = useState(String(source.timeout_s));
  const [enabled, setEnabled] = useState(source.enabled);
  const [err, setErr] = useState<string | null>(null);
  // A change made elsewhere shows once the page has the new values.
  useEffect(() => {
    setRpm(String(source.rpm)); setConc(String(source.max_concurrent)); setTimeoutS(String(source.timeout_s)); setEnabled(source.enabled);
  }, [source.rpm, source.max_concurrent, source.timeout_s, source.enabled]);

  const rpmV = wholeNumber(rpm, 0, 100000);
  const concV = wholeNumber(conc, 0, 1000);
  const timeoutV = wholeNumber(timeout, 1, 300);
  const body: Record<string, number | boolean> = {};
  if (rpmV !== null && rpmV !== source.rpm) body.rpm = rpmV;
  if (concV !== null && concV !== source.max_concurrent) body.max_concurrent = concV;
  if (timeoutV !== null && timeoutV !== source.timeout_s) body.timeout_s = timeoutV;
  if (enabled !== source.enabled) body.enabled = enabled;
  const invalid = rpmV === null || concV === null || timeoutV === null;

  const save = useMutation({
    mutationFn: () => apiFetch<ToolSource>(`/mcp/sources/${encodeURIComponent(source.slug)}`, { method: "PUT", body: JSON.stringify(body) }),
    onSuccess: async () => { setErr(null); toast.success("Saved."); await onSaved(); },
    onError: (e: unknown) => setErr(errText(e, "Couldn't save.")),
  });

  const limit = (n: number) => (n === 0 ? "no limit" : String(n));
  if (!canManage) {
    return (
      <section className="card" aria-label="Limits">
        <h2 className="card-title">Limits</h2>
        <dl className="kv-list">
          <div><dt>Calls per minute</dt><dd>{limit(source.rpm)}</dd></div>
          <div><dt>Calls at once</dt><dd>{limit(source.max_concurrent)}</dd></div>
          <div><dt>Timeout</dt><dd>{source.timeout_s} s</dd></div>
          <div><dt>Enabled</dt><dd>{source.enabled ? "Yes" : "No"}</dd></div>
        </dl>
      </section>
    );
  }
  return (
    <section className="card" aria-label="Limits">
      <h2 className="card-title">Limits</h2>
      <FormFieldGroup>
        <FormField label="Calls per minute" htmlFor="ts-rpm" w="sm" help="0: no limit.">
          <Input id="ts-rpm" inputMode="numeric" value={rpm} invalid={rpmV === null} onChange={(e) => setRpm(e.target.value)} />
        </FormField>
        <FormField label="Calls at once" htmlFor="ts-conc" w="sm" help="0: no limit. A call over the limit is refused, not queued.">
          <Input id="ts-conc" inputMode="numeric" value={conc} invalid={concV === null} onChange={(e) => setConc(e.target.value)} />
        </FormField>
        <FormField label="Timeout (seconds)" htmlFor="ts-timeout" w="sm" help="1 to 300.">
          <Input id="ts-timeout" inputMode="numeric" value={timeout} invalid={timeoutV === null} onChange={(e) => setTimeoutS(e.target.value)} />
        </FormField>
        <div className="row row-center gap-2">
          <Switch id="ts-enabled" checked={enabled} onChange={setEnabled} aria-label="Enabled" />
          <label htmlFor="ts-enabled">Enabled</label>
          <span className="muted small">A disabled source answers 404 and is not read in the background.</span>
        </div>
      </FormFieldGroup>
      {err && <ErrorNotice>{err}</ErrorNotice>}
      <div>
        <Button variant="primary" size="sm" disabled={invalid || Object.keys(body).length === 0 || save.isPending} onClick={() => { setErr(null); save.mutate(); }}>
          {save.isPending ? "Saving…" : "Save limits"}
        </Button>
      </div>
    </section>
  );
}
