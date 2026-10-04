import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import Guardrails from "@/pages/Guardrails";

function mount() {
  return renderApp(<Guardrails />, "/guardrails");
}

describe("Guardrails page (§4.22)", () => {
  it("renders three accordion sections (Regex, Presidio, Prompt-injection), collapsed by default", async () => {
    mount();
    expect(await screen.findByRole("button", { name: /regex redaction/i })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: /presidio/i })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: /prompt-injection/i })).toHaveAttribute("aria-expanded", "false");
  });

  it("Regex section shows built-in + custom tables when expanded", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /regex redaction/i }));
    expect(await screen.findByRole("table", { name: /built-in rules/i })).toBeInTheDocument();
    expect(screen.getByRole("table", { name: /custom rules/i })).toBeInTheDocument();
  });

  it("Presidio section shows the verbatim muted line and a Test connection button", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /presidio/i }));
    expect(
      await screen.findByText(
        "Runs Microsoft Presidio (Apache-2.0) as a sidecar process Burrow shells out to. Off by default — you install Presidio yourself.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /test connection/i })).toBeInTheDocument();
  });

  it("Prompt-injection section shows action Select + View pattern list disclosure", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /prompt-injection/i }));
    expect(await screen.findByLabelText(/on detection/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /view pattern list/i }));
    expect(await screen.findByText(/ignore previous instructions/i)).toBeInTheDocument();
  });

  it("Save Regex section issues PUT /redaction/settings and toasts", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /regex redaction/i }));
    await userEvent.click(await screen.findByRole("button", { name: /save regex/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/redaction/settings")
          && (init as RequestInit | undefined)?.method === "PUT",
        ),
      ).toBe(true);
    });
  });
});

describe("Guardrails custom rules (F9)", () => {
  async function openRegex() {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /regex redaction/i }));
  }

  it("adds a custom rule", async () => {
    await openRegex();
    await userEvent.click(screen.getByRole("button", { name: "New rule" }));
    const dialog = await screen.findByRole("dialog", { name: "New redaction rule" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "internal-id");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "ID-\\d{{6}");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    const table = await screen.findByRole("table", { name: "Custom rules" });
    expect(await within(table).findByText("internal-id")).toBeInTheDocument();
    expect(within(table).getByText("ID-\\d{6}")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "New redaction rule" })).toBeNull());
  });

  it("shows the server's validation message for an invalid regex", async () => {
    await openRegex();
    await userEvent.click(screen.getByRole("button", { name: "New rule" }));
    const dialog = await screen.findByRole("dialog", { name: "New redaction rule" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "bad");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "([[");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("invalid regex");
    expect(screen.getByRole("dialog", { name: "New redaction rule" })).toBeInTheDocument();
  });

  it("deletes a custom rule after confirmation", async () => {
    server.use(http.get("/api/v1/redaction/rules", () => HttpResponse.json({
      built_in: [],
      custom: [{ id: "r1", name: "internal-id", pattern: "ID-\\d+", action: "mask", scope: "both" }],
    })));
    let deleted = "";
    server.use(http.delete("/api/v1/redaction/rules/:id", ({ params }) => { deleted = String(params.id); return new HttpResponse(null, { status: 204 }); }));
    await openRegex();
    await userEvent.click(await screen.findByRole("button", { name: "Delete rule internal-id" }));
    const confirm = await screen.findByRole("dialog", { name: "Delete rule?" });
    await userEvent.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deleted).toBe("r1"));
  });

  it("offers no delete button on built-in rules", async () => {
    await openRegex();
    const builtIn = await screen.findByRole("table", { name: "Built-in rules" });
    // Non-vacuous: the table really holds the seeded built-in rows.
    expect(within(builtIn).getByText("Email address")).toBeInTheDocument();
    expect(within(builtIn).queryByRole("button", { name: /delete rule/i })).toBeNull();
  });
});
