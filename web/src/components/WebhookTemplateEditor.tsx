import { useRef, useState } from "react";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Select } from "@/components/ds";
import { WEBHOOK_EVENT_FIELDS } from "@/lib/contract";
import type { WebhookPreviewResponse } from "@/lib/contract";

export interface WebhookTemplateEditorValue {
  event: string;
  payload_template: string;
}

// Static sample field values for each known event — used when the operator clicks Preview.
const SAMPLE_FIELDS: Record<string, Record<string, unknown>> = {
  "ai.upstream_error": {
    service_id: "svc_ai001",
    backend_service_id: "svc_local",
    status: 503,
    error: "timeout",
    retry_count: 3,
  },
  "ai.cache_promotion": {
    service_id: "svc_ai001",
    exact_key_hash: "sha256:deadbeef1234",
    prompt_fingerprint: "fp_abc123",
    similarity_to_first: 0.97,
  },
  "audit.policy_change": {
    actor_email: "alice@acme.io",
    action: "roles.update",
    before: "{}",
    after: '{"permissions":["audit:read"]}',
  },
  "service.created": {
    service_id: "svc_new01",
    name: "my-new-service",
    type: "http",
    access_mode: "api_key",
  },
  "service.deleted": {
    service_id: "svc_old01",
    name: "old-service",
  },
  "connection.session_summary": {
    service_id: "svc_ai001",
    kind: "http_proxy",
    window_start: "2026-05-22T00:00:00Z",
    window_end: "2026-05-23T00:00:00Z",
    sessions: 42,
    bytes_in: 102400,
    bytes_out: 204800,
    avg_duration_ms: 350,
    p95_duration_ms: 1200,
    top_source_ips: ["203.0.113.7", "198.51.100.4"],
  },
  "audit.tokens.create": {
    actor_email: "alice@acme.io",
    token_name: "office-box-1",
  },
  "quota.exceeded": {
    scope: "service",
    subject: "svc_ai001",
    dimension: "rpm",
    limit: 100,
    window: "minute",
    service_id: "svc_ai001",
  },
  "budget.exceeded": {
    budget_id: "bdg_ci",
    scope: "api_key",
    subject_id: "sak_ci01",
    daily_usd: 10,
    current_usd: 10.5,
    daily_tokens: 0,
    current_tokens: 184200,
    action_on_exceed: "alert_webhook",
    // Set for the scopes gateway_key and model only; shown here so a
    // template that uses them previews with a value.
    gateway_key_id: "gwk_ci01",
    model: "burrow-smart",
  },
  "redaction.applied": {
    service_id: "svc_ai001",
    rule_id: "email",
    count: 2,
  },
  "tunnel.connected": {
    session_id: "sess_4f7a9c0b2e81",
    remote_addr: "203.0.113.7:51234",
  },
  "tunnel.disconnected": {
    session_id: "sess_4f7a9c0b2e81",
    reason: "client_closed",
  },
  "tunnel.failed": {
    session_id: "sess_fail01",
    reason: "auth_failed",
  },
  "cert.expiring": {
    service_id: "svc_web01",
    hostname: "tunnels.example.com",
    not_after: "2026-06-01T00:00:00Z",
    days_remaining: 9,
  },
};

export function WebhookTemplateEditor({
  webhookId,
  value,
  onChange,
  availableEvents,
}: {
  webhookId: string;
  value: WebhookTemplateEditorValue;
  onChange: (v: WebhookTemplateEditorValue) => void;
  availableEvents: string[];
}) {
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const [preview, setPreview] = useState<WebhookPreviewResponse | null>(null);
  const [previewErr, setPreviewErr] = useState<string | null>(null);
  const [previewPending, setPreviewPending] = useState(false);

  const eventFields = WEBHOOK_EVENT_FIELDS[value.event] ?? [];

  function insertField(field: string) {
    const ta = textareaRef.current;
    const snippet = `{{.${field}}}`;
    if (ta) {
      const start = ta.selectionStart ?? ta.value.length;
      const end = ta.selectionEnd ?? ta.value.length;
      const next =
        ta.value.slice(0, start) + snippet + ta.value.slice(end);
      onChange({ ...value, payload_template: next });
      // Restore focus + cursor after state update
      requestAnimationFrame(() => {
        ta.focus();
        ta.selectionStart = start + snippet.length;
        ta.selectionEnd = start + snippet.length;
      });
    } else {
      onChange({ ...value, payload_template: value.payload_template + snippet });
    }
  }

  async function handlePreview() {
    setPreviewErr(null);
    setPreview(null);
    setPreviewPending(true);
    try {
      const fields = SAMPLE_FIELDS[value.event] ?? {};
      const res = await apiFetch<WebhookPreviewResponse>(
        `/webhooks/${webhookId}/preview`,
        {
          method: "POST",
          body: JSON.stringify({
            event: value.event,
            fields,
            payload_template: value.payload_template,
          }),
        },
      );
      setPreview(res);
    } catch (e: unknown) {
      setPreviewErr(
        e instanceof ApiError ? e.message : "Preview failed.",
      );
    } finally {
      setPreviewPending(false);
    }
  }

  const eventOptions = availableEvents.map((ev) => ({ value: ev, label: ev }));

  return (
    <div className="webhook-template-editor">
      {/* Event selector */}
      {availableEvents.length > 0 && (
        <div className="field">
          <label htmlFor="wte-event">Event</label>
          <Select
            id="wte-event"
            options={eventOptions}
            value={value.event}
            onChange={(ev) => onChange({ ...value, event: ev })}
          />
        </div>
      )}

      {/* Insert field dropdown — rendered as a simple inline selector */}
      {eventFields.length > 0 && (
        <div className="field">
          <label htmlFor="wte-insert-field">Insert field</label>
          <select
            id="wte-insert-field"
            className="select-trigger"
            value=""
            onChange={(e) => {
              if (e.target.value) insertField(e.target.value);
              e.target.value = "";
            }}
            aria-label="Insert field"
          >
            <option value="" disabled>
              — pick a field to insert —
            </option>
            {eventFields.map((f) => (
              <option key={f} value={f}>
                {f}
              </option>
            ))}
          </select>
        </div>
      )}

      {/* Payload template textarea */}
      <div className="field">
        <label htmlFor="wte-tpl">Payload template</label>
        <textarea
          ref={textareaRef}
          id="wte-tpl"
          aria-label="Payload template"
          className="input mono resizable"
          rows={8}
          value={value.payload_template}
          onChange={(e) =>
            onChange({ ...value, payload_template: e.target.value })
          }
        />
        <p className="muted small">
          Templates run in a sandbox — no file, network, or environment
          access. See docs for the function allowlist.
        </p>
      </div>

      {/* Preview button + output */}
      <div className="row row-center gap-2">
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void handlePreview()}
          disabled={previewPending}
        >
          {previewPending ? "Previewing…" : "Preview"}
        </Button>
      </div>

      {previewErr && (
        <div role="alert" className="notice-inline">
          {previewErr}
        </div>
      )}

      {preview && (
        <div>
          <p className="muted small">{preview.size_bytes} bytes</p>
          <pre className="preview-block">{preview.rendered}</pre>
        </div>
      )}
    </div>
  );
}
