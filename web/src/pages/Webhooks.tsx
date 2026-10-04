import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Badge, Button, Checkbox, Dialog, DropdownMenu, FormField, FormFieldGroup, Input, PageHeader, SkeletonRows } from "@/components/ds";
import { WebhookTemplateEditor } from "@/components/WebhookTemplateEditor";
import type { WebhookTemplateEditorValue } from "@/components/WebhookTemplateEditor";
import type { CreatedWebhook, Webhook, WebhookDelivery } from "@/lib/contract";

// Subscribable webhook events. MUST stay in sync with the backend closed
// vocabulary `ClosedEvents` in internal/webhook/dispatcher.go — the API rejects
// any event not in that set. (`webhook.test` is intentionally omitted: it is
// fired directly by the "Send test event" action, not via subscription.)
const WEBHOOK_EVENTS = [
  "tunnel.connected",
  "tunnel.disconnected",
  "tunnel.failed",
  "access.denied",
  "quota.exceeded",
  "budget.exceeded",
  "redaction.applied",
  "guardrail.refused",
  "cert.expiring",
  "audit.exported",
  "backup.completed",
  "ai.upstream_error",
  "ai.cache_promotion",
  "audit.policy_change",
  "service.created",
  "service.deleted",
  "connection.session_summary",
  "custom_domain.cert.expiring",
];

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
}

// Multi-select events picker using Checkbox DS component.
function EventsPicker({
  selected,
  onChange,
}: {
  selected: string[];
  onChange: (events: string[]) => void;
}) {
  function toggle(ev: string) {
    if (selected.includes(ev)) {
      onChange(selected.filter((e) => e !== ev));
    } else {
      onChange([...selected, ev]);
    }
  }

  return (
    <div className="form-field-group events-picker">
      {WEBHOOK_EVENTS.map((ev) => (
        <label key={ev} className="checkbox-row small">
          <Checkbox
            id={`ev-${ev}`}
            checked={selected.includes(ev)}
            onChange={() => toggle(ev)}
          />
          <span className="mono">{ev}</span>
        </label>
      ))}
    </div>
  );
}

export default function Webhooks() {
  const qc = useQueryClient();
  const list = useQuery({
    queryKey: ["webhooks"],
    queryFn: () => apiFetch<Webhook[]>("/webhooks"),
    retry: false,
  });
  const deliveries = useQuery({
    queryKey: ["webhook-deliveries"],
    queryFn: () => apiFetch<WebhookDelivery[]>("/webhooks/deliveries?limit=50"),
    retry: false,
  });

  // ---- Add dialog state ----
  const [addOpen, setAddOpen] = useState(false);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [addEvents, setAddEvents] = useState<string[]>([WEBHOOK_EVENTS[0]]);
  const [createdSecret, setCreatedSecret] = useState<{ webhook: Webhook; signing_secret: string } | null>(null);
  const [err, setErr] = useState<string | null>(null);

  // ---- Edit dialog state ----
  const [editWebhook, setEditWebhook] = useState<Webhook | null>(null);
  const [editUrl, setEditUrl] = useState("");
  const [editEvents, setEditEvents] = useState<string[]>([]);
  const [editTemplate, setEditTemplate] = useState<WebhookTemplateEditorValue>({ event: "", payload_template: "" });
  const [editErr, setEditErr] = useState<string | null>(null);

  function openEdit(w: Webhook) {
    setEditWebhook(w);
    setEditUrl(w.url);
    setEditEvents(w.events.length > 0 ? w.events : [WEBHOOK_EVENTS[0]]);
    const wAny = w as unknown as Record<string, unknown>;
    setEditTemplate({
      event: w.events[0] ?? WEBHOOK_EVENTS[0],
      payload_template: typeof wAny["payload_template"] === "string" ? wAny["payload_template"] : "",
    });
    setEditErr(null);
  }

  const create = useMutation({
    mutationFn: () =>
      apiFetch<CreatedWebhook>("/webhooks", {
        method: "POST",
        body: JSON.stringify({ name, url, events: addEvents }),
      }),
    onSuccess: (res) => {
      setCreatedSecret(res);
      qc.invalidateQueries({ queryKey: ["webhooks"] });
      setAddOpen(false);
      setName("");
      setUrl("");
      setAddEvents([WEBHOOK_EVENTS[0]]);
      setErr(null);
    },
    onError: (e: unknown) =>
      setErr(e instanceof ApiError ? e.message : "Couldn't create webhook."),
  });

  const update = useMutation({
    mutationFn: () =>
      apiFetch<void>(`/webhooks/${editWebhook!.id}`, {
        method: "PUT",
        body: JSON.stringify({
          // The backend PutWebhook validator requires a non-empty `name`
          // (internal/api/webhook_handlers.go validateWebhookReq) and persists
          // it verbatim, so we must echo the existing name on edit — otherwise
          // every save 400s with "name is required" (and would blank the name).
          name: editWebhook!.name,
          url: editUrl,
          events: editEvents,
          payload_template: editTemplate.payload_template,
        }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["webhooks"] });
      setEditWebhook(null);
      toast.success("Webhook updated.");
    },
    onError: (e: unknown) =>
      setEditErr(e instanceof ApiError ? e.message : "Couldn't update webhook."),
  });

  const pause = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/webhooks/${id}/pause`, { method: "POST", body: "{}" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["webhooks"] }),
  });
  const resume = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/webhooks/${id}/resume`, { method: "POST", body: "{}" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["webhooks"] }),
  });
  const remove = useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/webhooks/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["webhooks"] }),
  });

  function submit() {
    setErr(null);
    try {
      const u = new URL(url);
      if (u.protocol !== "https:") {
        setErr("URL must use https://");
        return;
      }
    } catch {
      setErr("URL must use https://");
      return;
    }
    create.mutate();
  }

  function submitEdit() {
    setEditErr(null);
    try {
      const u = new URL(editUrl);
      if (u.protocol !== "https:") {
        setEditErr("URL must use https://");
        return;
      }
    } catch {
      setEditErr("URL must use https://");
      return;
    }
    update.mutate();
  }

  function statusOf(w: Webhook): { kind: string; text: string } {
    if (w.paused) return { kind: "status-paused", text: "Paused" };
    if (w.consecutive_failures >= 3) return { kind: "status-failing", text: "Failing" };
    return { kind: "status-connected", text: "Healthy" };
  }

  return (
    <div className="webhooks-page">
      <PageHeader
        title="Webhooks"
        subtitle={<>Burrow signs every webhook with an HMAC-SHA256 signature in the{" "}<code className="mono">Burrow-Signature</code> header. Verify on receipt.{" "}<a href="/docs/webhooks" className="link-inline">Docs</a></>}
        actions={<Button variant="primary" size="sm" onClick={() => setAddOpen(true)}>Add webhook</Button>}
      />

      {/* P0-10: render the Webhooks table unconditionally; the empty state
          becomes an in-table row so Playwright + the page-head CTA agree on
          the same affordance (P2-4 dedup). */}
      <section className="card">
        <h2>Configured webhooks</h2>
      <div className="table-wrap">
        <table className="data" aria-label="Webhooks">
          <thead>
            <tr><th>Name</th><th>URL</th><th>Status</th><th>Failures</th><th className="col-actions"></th></tr>
          </thead>
          <tbody>
            {!list.data ? (
              <tr><td colSpan={5}><SkeletonRows n={3} /></td></tr>
            ) : list.data.length === 0 ? (
              <tr>
                <td colSpan={5} className="muted">
                  No webhooks yet. Burrow can POST to a URL when audit events,
                  AI requests, or other signals fire.
                </td>
              </tr>
            ) : (
              list.data.map((w) => {
                const s = statusOf(w);
                return (
                  <tr key={w.id}>
                    <td>{w.name}</td>
                    <td>
                      <span className="row row-center gap-2">
                        <span className="mono small truncate">{w.url}</span>
                        <button
                          type="button"
                          className="icon-btn"
                          aria-label="Copy webhook URL"
                          onClick={() => copy(w.url)}
                        >
                          <Copy size={13} />
                        </button>
                      </span>
                    </td>
                    <td><Badge kind={s.kind}>{s.text}</Badge></td>
                    <td className="mono">{w.consecutive_failures}</td>
                    <td className="col-actions">
                      <DropdownMenu
                        trigger={<button type="button" className="icon-btn" aria-label={`Actions for ${w.name}`}>⋯</button>}
                        items={[
                          { label: "Edit", onSelect: () => openEdit(w) },
                          w.paused
                            ? { label: "Resume", onSelect: () => resume.mutate(w.id) }
                            : { label: "Pause", onSelect: () => pause.mutate(w.id) },
                          { label: "Send test event", onSelect: () => { void apiFetch(`/webhooks/${w.id}/test`, { method: "POST", body: "{}" }); } },
                          { label: "Delete", danger: true, onSelect: () => remove.mutate(w.id) },
                        ]}
                      />
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>
      </section>

      <section className="card">
        <h2>Recent deliveries</h2>
        <div className="table-wrap">
          <table className="data" aria-label="Webhook deliveries">
            <thead>
              <tr><th>When</th><th>Event</th><th>Status</th><th>Latency</th></tr>
            </thead>
            <tbody>
              {!deliveries.data ? (
                <tr><td colSpan={4}><SkeletonRows n={3} /></td></tr>
              ) : deliveries.data.length === 0 ? (
                <tr>
                  <td colSpan={4} className="muted">
                    No deliveries yet. Burrow records each webhook POST and its response here.
                  </td>
                </tr>
              ) : (
                deliveries.data.map((d) => (
                  <tr key={d.id}>
                    <td className="mono small">{d.ts}</td>
                    <td className="mono">{d.event}</td>
                    <td className="mono">{d.status_code}</td>
                    <td className="mono">{d.latency_ms} ms</td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </section>

      {/* Add webhook dialog */}
      <Dialog
        open={addOpen}
        onOpenChange={(o) => { setAddOpen(o); if (!o) setErr(null); }}
        size="md"
        title="Add webhook"
        footer={
          <>
            <Button variant="secondary" onClick={() => setAddOpen(false)}>Cancel</Button>
            <Button variant="primary" disabled={create.isPending} onClick={submit}>Create</Button>
          </>
        }
      >
        <FormFieldGroup>
          <FormField label="Name" htmlFor="wh-name" w="md">
            <Input id="wh-name" value={name} onChange={(e) => setName(e.target.value)} />
          </FormField>
          <FormField label="URL" htmlFor="wh-url" w="lg">
            <Input id="wh-url" className="mono" type="url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://example.com/hook" />
          </FormField>
        </FormFieldGroup>
        <div className="field">
          <label>Events</label>
          <EventsPicker selected={addEvents} onChange={setAddEvents} />
        </div>
        {err && <p role="alert" className="notice-inline error">{err}</p>}
      </Dialog>

      {/* Edit webhook dialog */}
      <Dialog
        open={editWebhook !== null}
        onOpenChange={(o) => { if (!o) { setEditWebhook(null); setEditErr(null); } }}
        size="md"
        title={`Edit webhook: ${editWebhook?.name ?? ""}`}
        footer={
          <>
            <Button variant="secondary" onClick={() => setEditWebhook(null)}>Cancel</Button>
            <Button variant="primary" disabled={update.isPending} onClick={submitEdit}>Save</Button>
          </>
        }
      >
        <div className="field">
          <label htmlFor="edit-wh-url">URL</label>
          <Input
            id="edit-wh-url"
            className="mono"
            value={editUrl}
            onChange={(e) => setEditUrl(e.target.value)}
            placeholder="https://example.com/hook"
          />
        </div>
        <div className="field">
          <label>Events</label>
          <EventsPicker selected={editEvents} onChange={setEditEvents} />
        </div>
        {editWebhook && (
          <div className="field">
            <label>Payload template</label>
            <WebhookTemplateEditor
              webhookId={editWebhook.id}
              value={editTemplate}
              onChange={setEditTemplate}
              availableEvents={editEvents.length > 0 ? editEvents : WEBHOOK_EVENTS}
            />
          </div>
        )}
        {editErr && <p role="alert" className="notice-inline error">{editErr}</p>}
      </Dialog>

      {/* Signing secret reveal dialog */}
      <Dialog
        open={createdSecret !== null}
        onOpenChange={(o) => { if (!o) setCreatedSecret(null); }}
        title="Signing secret"
        footer={
          <Button variant="primary" onClick={() => setCreatedSecret(null)}>
            I've saved it
          </Button>
        }
      >
        <p>Save this signing secret now — you won't see it again.</p>
        <div className="row row-center gap-2">
          <code className="mono small">{createdSecret?.signing_secret}</code>
          <button
            type="button"
            className="icon-btn"
            aria-label="Copy signing secret"
            onClick={() => { if (createdSecret) { copy(createdSecret.signing_secret); toast.success("Copied."); } }}
          >
            <Copy size={13} />
          </button>
        </div>
      </Dialog>
      <Toaster />
    </div>
  );
}
