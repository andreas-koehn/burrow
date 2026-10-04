import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import Webhooks from "@/pages/Webhooks";

function mount() {
  return renderApp(<Webhooks />, "/webhooks");
}

describe("Webhooks (§4.26)", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders the verbatim HMAC preamble", async () => {
    mount();
    expect(
      await screen.findByText(
        /Burrow signs every webhook with an HMAC-SHA256 signature in the/,
      ),
    ).toBeInTheDocument();
  });

  it("renders a row per webhook with a mono URL + Copy affordance", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /webhooks/i });
    // P0-10: the table is now always-on; wait for the seeded row to land
    // before asserting on its contents (initial render shows a skeleton row).
    await waitFor(() => {
      expect(within(table).getByText(/example\.com\/hook/)).toBeInTheDocument();
    });
    expect(within(table).getByRole("button", { name: /copy webhook url/i })).toBeInTheDocument();
  });

  it("New webhook rejects non-HTTPS URLs and reveals the signing secret on success", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new webhook/i }));
    const name = await screen.findByLabelText(/^name$/i);
    const url = screen.getByLabelText(/^url$/i);
    await userEvent.type(name, "ops");
    await userEvent.type(url, "http://example.com/x");
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/https/i);
    await userEvent.clear(url);
    await userEvent.type(url, "https://example.com/ops");
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([u, init]) =>
          String(u).endsWith("/api/v1/webhooks")
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
    expect(
      await screen.findByText("Save this signing secret now — you won't see it again."),
    ).toBeInTheDocument();
  });

  it("Add Dialog events picker includes the v0.5.0 events", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new webhook/i }));

    // Wait for dialog to appear — identify by the dialog heading specifically
    await screen.findByRole("heading", { name: /new webhook/i });

    // All 6 v0.5.0 event checkboxes should be present (as text labels)
    const v5Events = [
      "ai.upstream_error",
      "ai.cache_promotion",
      "audit.policy_change",
      "service.created",
      "service.deleted",
      "connection.session_summary",
    ];

    for (const ev of v5Events) {
      // getAllByText to avoid "multiple elements" error in case there are multiple matches
      const matches = screen.getAllByText(ev);
      expect(matches.length).toBeGreaterThan(0);
    }
  });

  // P0-10: the empty state now lives inside the always-on Webhooks table
  // so Playwright (and the table-aware assertions in the page) can locate
  // the table even on a fresh stack. The C3 EmptyState card is intentionally
  // gone — we keep the in-table row that calls out "No webhooks yet" and
  // rely on the page-head New webhook CTA (P2-4 dedup).
  it("renders an in-table empty row when there are no webhooks (P0-10)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.includes("/webhooks/deliveries")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      if (u.includes("/webhooks")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      return new Response("[]", { status: 200 }) as Response;
    });
    mount();
    const table = await screen.findByRole("table", { name: /webhooks/i });
    await waitFor(() => {
      expect(within(table).getByText(/No webhooks yet/i)).toBeInTheDocument();
    });
  });

  it("shows an empty-state row in Recent deliveries when there are none (D-7/L-9)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.includes("/webhooks/deliveries")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      if (u.includes("/webhooks")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      return new Response("[]", { status: 200 }) as Response;
    });
    renderApp(<Webhooks />);
    expect(await screen.findByText(/no deliveries yet/i)).toBeInTheDocument();
  });

  it("Docs link has link-inline class so it reads as clickable (C4)", async () => {
    mount();
    await waitFor(() => screen.getByRole("heading", { name: "Webhooks" }));
    const docs = screen.getByRole("link", { name: /docs/i });
    expect(docs.className).toContain("link-inline");
  });

  it("Edit menu opens dialog with template editor + Save → PUT /webhooks/:id", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();

    // Find the row's action menu and click Edit
    const actionsBtn = await screen.findByRole("button", { name: /actions for ops-pager/i });
    await userEvent.click(actionsBtn);

    const editItem = await screen.findByRole("menuitem", { name: /^edit$/i });
    await userEvent.click(editItem);

    // Edit dialog should open
    await screen.findByText(/edit webhook/i);

    // The URL input and template editor should be visible
    const urlInput = screen.getByLabelText(/^url$/i);
    expect(urlInput).toBeInTheDocument();

    // The Payload template textarea from WebhookTemplateEditor should be visible
    expect(
      await screen.findByRole("textbox", { name: /payload template/i }),
    ).toBeInTheDocument();

    // Type a template
    const tplTextarea = screen.getByRole("textbox", { name: /payload template/i });
    await userEvent.clear(tplTextarea);
    await userEvent.type(tplTextarea, "Service: {{.service_id}}");

    // Save
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    // Verify PUT fired with body containing url, events, payload_template
    await waitFor(() => {
      const putCalls = fetchSpy.mock.calls.filter(([u, init]) =>
        String(u).includes("/api/v1/webhooks/wh_ops") &&
        (init as RequestInit | undefined)?.method === "PUT",
      );
      expect(putCalls.length).toBeGreaterThan(0);
      const [, init] = putCalls[0]!;
      const bodyStr = (init as RequestInit).body as string;
      const parsed = JSON.parse(bodyStr) as Record<string, unknown>;
      expect(parsed).toHaveProperty("url");
      expect(parsed).toHaveProperty("events");
      expect(parsed).toHaveProperty("payload_template");
    });
  });
});
