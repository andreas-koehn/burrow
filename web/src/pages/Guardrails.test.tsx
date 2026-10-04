import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
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
  // fetch spies are shared across tests unless restored; the cancel case
  // counts DELETE calls and must not see an earlier test's request.
  afterEach(() => { vi.restoreAllMocks(); });

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

  const SEEDED = { id: "r1", name: "internal-id", pattern: "ID-\\d+", action: "mask", scope: "both" } as const;
  const deleteCalls = (spy: { mock: { calls: unknown[][] } }) =>
    spy.mock.calls.filter(([url, init]) =>
      String(url).includes("/api/v1/redaction/rules/")
      && (init as RequestInit | undefined)?.method === "DELETE");

  it("deletes a custom rule after confirmation: dialog closes and the row is removed", async () => {
    db.redactionRules.custom.push({ ...SEEDED });
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    await openRegex();
    await userEvent.click(await screen.findByRole("button", { name: "Delete rule internal-id" }));
    const confirm = await screen.findByRole("dialog", { name: "Delete rule?" });
    await userEvent.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Delete rule?" })).toBeNull());
    const table = screen.getByRole("table", { name: "Custom rules" });
    await waitFor(() => expect(within(table).queryByText("internal-id")).toBeNull());
    expect(within(table).getByText("No rules yet.")).toBeInTheDocument();
    expect(deleteCalls(fetchSpy)).toHaveLength(1);
    expect(String(deleteCalls(fetchSpy)[0][0])).toMatch(/\/redaction\/rules\/r1$/);
  });

  it("cancelling the delete confirmation sends no request and keeps the row", async () => {
    db.redactionRules.custom.push({ ...SEEDED });
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    await openRegex();
    await userEvent.click(await screen.findByRole("button", { name: "Delete rule internal-id" }));
    const confirm = await screen.findByRole("dialog", { name: "Delete rule?" });
    await userEvent.click(within(confirm).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Delete rule?" })).toBeNull());
    expect(within(screen.getByRole("table", { name: "Custom rules" })).getByText("internal-id")).toBeInTheDocument();
    expect(deleteCalls(fetchSpy)).toHaveLength(0);
  });

  it("Enter in a text field submits the new-rule form", async () => {
    await openRegex();
    await userEvent.click(screen.getByRole("button", { name: "New rule" }));
    const dialog = await screen.findByRole("dialog", { name: "New redaction rule" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "by-enter");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "abc{Enter}");
    const table = await screen.findByRole("table", { name: "Custom rules" });
    expect(await within(table).findByText("by-enter")).toBeInTheDocument();
  });

  // A stalled POST must not trap the user: every exit closes the dialog and
  // the late result is dropped instead of landing in a reopened form.
  it.each([
    ["Cancel", "Cancel"],
    ["the header X", "Close dialog"],
  ])("closes via %s while the create is pending and drops the late result", async (_label, button) => {
    let release!: () => void;
    const held = new Promise<void>((r) => { release = r; });
    let answered = false;
    server.use(http.post("/api/v1/redaction/rules", async () => {
      await held;
      answered = true;
      return HttpResponse.json({ error: "invalid regex" }, { status: 400 });
    }));
    await openRegex();
    await userEvent.click(screen.getByRole("button", { name: "New rule" }));
    const dialog = await screen.findByRole("dialog", { name: "New redaction rule" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "slow");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "abc");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByRole("button", { name: "Creating…" })).toBeDisabled();
    await userEvent.click(within(dialog).getByRole("button", { name: button }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "New redaction rule" })).toBeNull());

    await userEvent.click(screen.getByRole("button", { name: "New rule" }));
    const reopened = await screen.findByRole("dialog", { name: "New redaction rule" });
    expect(within(reopened).getByLabelText("Name")).toHaveValue("");
    expect(within(reopened).getByLabelText("Pattern")).toHaveValue("");
    release();
    await waitFor(() => expect(answered).toBe(true));
    // Give the rejected request time to reach (and be ignored by) the dialog.
    await new Promise((r) => setTimeout(r, 50));
    expect(within(reopened).queryByRole("alert")).toBeNull();
    expect(within(reopened).getByLabelText("Name")).toHaveValue("");
    expect(within(reopened).getByRole("button", { name: "Create" })).toBeInTheDocument();
  });

  it("names the rule in the success toast from the server response", async () => {
    server.use(http.post("/api/v1/redaction/rules", () => HttpResponse.json(
      { id: "r9", name: "server-name", pattern: "abc", action: "mask", scope: "both" }, { status: 201 },
    )));
    await openRegex();
    await userEvent.click(screen.getByRole("button", { name: "New rule" }));
    const dialog = await screen.findByRole("dialog", { name: "New redaction rule" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "typed-name");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "abc");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await screen.findByText("Rule server-name added.")).toBeInTheDocument();
  });

  it("offers no delete button on built-in rules", async () => {
    await openRegex();
    const builtIn = await screen.findByRole("table", { name: "Built-in rules" });
    // Non-vacuous: the table really holds the seeded built-in rows.
    expect(within(builtIn).getByText("Email address")).toBeInTheDocument();
    expect(within(builtIn).queryByRole("button", { name: /delete rule/i })).toBeNull();
  });
});
