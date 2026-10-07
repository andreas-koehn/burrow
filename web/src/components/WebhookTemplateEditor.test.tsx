import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { WebhookTemplateEditor } from "@/components/WebhookTemplateEditor";
import { WEBHOOK_EVENT_FIELDS } from "@/lib/contract";

const ALL_EVENTS = Object.keys(WEBHOOK_EVENT_FIELDS);

function mountEditor(
  webhookId = "wh_ops",
  initialEvent = "ai.upstream_error",
  initialTemplate = "",
) {
  let currentValue = { event: initialEvent, payload_template: initialTemplate };
  const onChange = vi.fn((v: typeof currentValue) => { currentValue = v; });

  const { rerender } = renderApp(
    <WebhookTemplateEditor
      webhookId={webhookId}
      value={currentValue}
      onChange={onChange}
      availableEvents={ALL_EVENTS}
    />,
    "/webhooks",
  );

  // Helper to re-render with the latest onChange value
  function applyChange() {
    rerender(
      <WebhookTemplateEditor
        webhookId={webhookId}
        value={currentValue}
        onChange={onChange}
        availableEvents={ALL_EVENTS}
      />,
    );
  }

  return { onChange, applyChange };
}

describe("WebhookTemplateEditor", () => {
  it("renders the textarea with aria-label='Payload template'", async () => {
    mountEditor();
    const ta = await screen.findByRole("textbox", { name: /payload template/i });
    expect(ta).toBeInTheDocument();
    expect(ta.tagName.toLowerCase()).toBe("textarea");
  });

  it("renders the honesty disclosure verbatim", async () => {
    mountEditor();
    expect(
      await screen.findByText(
        /Templates run in a sandbox — no file, network, or environment access\. See docs for the function allowlist\./,
      ),
    ).toBeInTheDocument();
  });

  it("Insert field dropdown lists the WEBHOOK_EVENT_FIELDS for the selected event", async () => {
    mountEditor("wh_ops", "ai.upstream_error");
    // The native <select> with aria-label="Insert field" has combobox role
    const insertSelect = await screen.findByRole("combobox", { name: /insert field/i });
    expect(insertSelect).toBeInTheDocument();

    // All 5 fields for ai.upstream_error should be present as options
    const expectedFields = ["service_id", "backend_service_id", "status", "error", "retry_count"];
    for (const field of expectedFields) {
      expect(screen.getByRole("option", { name: field })).toBeInTheDocument();
    }
  });

  it("Preview button POSTs to /webhooks/:id/preview and renders the rendered body", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mountEditor("wh_ops", "ai.upstream_error", "Service: {{.service_id}} status {{.status}}");

    const previewBtn = await screen.findByRole("button", { name: /preview/i });
    await userEvent.click(previewBtn);

    // Wait for the POST to fire
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([u, init]) =>
          String(u).includes("/api/v1/webhooks/wh_ops/preview") &&
          (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });

    // The rendered body from the MSW handler should appear in a <pre>
    // <pre> has no ARIA role, find by the rendered text content
    await waitFor(() => {
      // After preview the <pre> should have text like "Service: svc_ai001 status 503"
      const pre = document.querySelector("pre");
      expect(pre).toBeInTheDocument();
      expect(pre?.textContent).toMatch(/svc_ai001|503|Service/);
    });
  });

  // The sample a preview sends mirrors the payload the relay publishes
  // (cmd/server/quota_events.go, internal/cost/engine.go fireAction).
  it.each([
    ["quota.exceeded", ["dimension", "limit", "scope", "service_id", "subject", "window"]],
    ["budget.exceeded", [
      "action_on_exceed", "budget_id", "current_tokens", "current_usd", "daily_tokens", "daily_usd",
      "gateway_key_id", "model", "scope", "subject_id",
    ]],
  ])("Preview of %s sends the fields the relay publishes", async (event, fields) => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    fetchSpy.mockClear(); // an earlier test's spy may still hold its calls
    mountEditor("wh_ops", event, "{{.scope}}");
    await userEvent.click(await screen.findByRole("button", { name: /preview/i }));
    let sent: Record<string, unknown> | undefined;
    await waitFor(() => {
      const call = fetchSpy.mock.calls.find(([u, init]) =>
        String(u).includes("/api/v1/webhooks/wh_ops/preview") &&
        (init as RequestInit | undefined)?.method === "POST",
      );
      expect(call).toBeDefined();
      sent = JSON.parse(String((call![1] as RequestInit).body));
    });
    expect(sent!.event).toBe(event);
    const sample = sent!.fields as Record<string, unknown>;
    expect(Object.keys(sample).sort()).toEqual(fields);
    for (const [name, value] of Object.entries(sample)) {
      expect(value, name).not.toBe("");
      expect(value, name).not.toBeUndefined();
    }
    fetchSpy.mockRestore();
  });

  it("Server template error shows as inline alert with verbatim text", async () => {
    // Use a template that triggers the forbidden-template check in the mock
    mountEditor("wh_ops", "ai.upstream_error", '{{template "x"}}forbidden');

    const previewBtn = await screen.findByRole("button", { name: /preview/i });
    await userEvent.click(previewBtn);

    const alert = await screen.findByRole("alert");
    expect(alert).toBeInTheDocument();
    // The error message from the MSW mock should mention "nested template includes"
    expect(alert.textContent).toMatch(/nested template includes are forbidden/i);
  });
});
