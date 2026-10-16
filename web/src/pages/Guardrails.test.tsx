import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { ALL_PERMISSIONS } from "@/auth/permissions";
import type { GuardrailSettings } from "@/lib/contract";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import Guardrails from "@/pages/Guardrails";

function mount() {
  return renderApp(<Guardrails />, "/gateway/guardrails");
}

describe("Guardrails page (§4.22)", () => {
  it("renders the Regex, Presidio and Requests accordion sections, collapsed by default", async () => {
    mount();
    expect(await screen.findByRole("button", { name: /regex redaction/i })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: /presidio/i })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: /^requests/i })).toHaveAttribute("aria-expanded", "false");
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

  it("Requests section shows action Select + View pattern list disclosure", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /^requests/i }));
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
    // The pattern is behind the rule's name.
    await userEvent.click(within(table).getByRole("button", { name: "Show pattern of internal-id" }));
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

  it("the rules table says what a rule does and what it applies to, in plain words", async () => {
    db.redactionRules.custom.push(
      { id: "r1", name: "internal-id", pattern: "ID-\\d+", action: "mask", scope: "both" },
      { id: "r2", name: "secrets", pattern: "sk-[a-z0-9]+", action: "drop", scope: "request_body" },
      { id: "r3", name: "answers", pattern: "answer-[0-9]+", action: "hash", scope: "response_body" },
    );
    await openRegex();
    const table = await screen.findByRole("table", { name: "Custom rules" });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Name", "What it does", "Applies to", ""]);
    const cells = (name: string) => within(within(table).getByText(name).closest("tr")!).getAllByRole("cell").slice(1, 3).map((c) => c.textContent);
    expect(cells("internal-id")).toEqual(["Replaces the match with a placeholder", "Requests and responses"]);
    expect(cells("secrets")).toEqual(["Refuses the request", "Requests"]);
    expect(cells("answers")).toEqual(["Replaces the match with its hash", "Responses"]);
    // The words of the wire are not shown.
    expect(table.textContent).not.toMatch(/\b(mask|drop|both|request_body|response_body)\b/);
    expect(within(screen.getByRole("table", { name: "Built-in rules" })).getAllByRole("columnheader").map((h) => h.textContent))
      .toEqual(["Name", "What it does", "Applies to"]);
  });

  it("a pattern is not in the document until its row is expanded", async () => {
    db.redactionRules.custom.push(
      { id: "r1", name: "internal-id", pattern: "ID-\\d+", action: "mask", scope: "both" },
      { id: "r2", name: "secrets", pattern: "sk-[a-z0-9]+", action: "drop", scope: "request_body" },
    );
    await openRegex();
    const table = await screen.findByRole("table", { name: "Custom rules" });
    expect(within(table).queryByText("ID-\\d+")).toBeNull();
    expect(within(table).queryByText("sk-[a-z0-9]+")).toBeNull();
    const toggle = within(table).getByRole("button", { name: "Show pattern of internal-id" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveTextContent("Show pattern");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    // Its own row only, in the mono face, in the cell that wraps a long pattern.
    const pattern = within(table).getByText("ID-\\d+");
    expect(pattern).toHaveClass("mono");
    expect(pattern.closest("td")).toHaveClass("cell-wrap");
    expect(pattern.closest("tr")).toHaveTextContent("internal-id");
    expect(within(table).queryByText("sk-[a-z0-9]+")).toBeNull();
    expect(toggle).toHaveAccessibleName("Hide pattern of internal-id");
    await userEvent.click(toggle);
    expect(within(table).queryByText("ID-\\d+")).toBeNull();
    // The built-in rules keep theirs closed too.
    const builtIn = screen.getByRole("table", { name: "Built-in rules" });
    expect(builtIn.textContent).not.toContain("[A-Za-z0-9._%+-]+@");
    await userEvent.click(within(builtIn).getByRole("button", { name: "Show pattern of Email address" }));
    expect(builtIn.textContent).toContain("[A-Za-z0-9._%+-]+@");
  });

  it("offers no delete button on built-in rules", async () => {
    await openRegex();
    const builtIn = await screen.findByRole("table", { name: "Built-in rules" });
    // Non-vacuous: the table really holds the seeded built-in rows.
    expect(within(builtIn).getByText("Email address")).toBeInTheDocument();
    expect(within(builtIn).queryByRole("button", { name: /delete rule/i })).toBeNull();
  });
});

describe("Guardrails: answers, custom patterns and the judge (G07)", () => {
  const SEEDED: GuardrailSettings = {
    enabled: true,
    action: "refuse_403",
    answers: { enabled: true, action: "flag" },
    custom_patterns: [
      { id: "codename", pattern: "project\\s+x", direction: "both", description: "An internal codename" },
      { id: "only-answers", pattern: "internal-only", direction: "answer", description: "Never in an answer" },
    ],
    judge: { enabled: false, model: "", policy: "No legal advice.", check: "requests", timeout_ms: 3000, on_failure: "allow" },
  };

  // The blobs the page PUT, in order.
  let puts: GuardrailSettings[];
  beforeEach(() => {
    db.guardrailSettings.global = structuredClone(SEEDED);
    puts = [];
    server.use(http.put("/api/v1/guardrails/settings", async ({ request }) => {
      const b = (await request.json()) as GuardrailSettings;
      puts.push(b);
      db.guardrailSettings = { ...db.guardrailSettings, global: b };
      return new HttpResponse(null, { status: 204 });
    }));
  });

  async function open(name: RegExp) {
    mount();
    await userEvent.click(await screen.findByRole("button", { name }));
  }
  async function openMore(name: RegExp) {
    await userEvent.click(await screen.findByRole("button", { name }));
  }

  it("shows the four sections with their headings", async () => {
    mount();
    for (const name of [/^requests/i, /^answers/i, /^custom patterns/i, /^judge/i]) {
      expect(await screen.findByRole("button", { name })).toHaveAttribute("aria-expanded", "false");
    }
  });

  it("Requests: says the switch applies to all model traffic, and lists your request patterns", async () => {
    await open(/^requests/i);
    expect(screen.getByText(/applies to all model traffic, unless a service turns on its own guardrails/i)).toBeInTheDocument();
    expect(screen.queryByText(/endpoint/i)).toBeNull();
    expect(screen.getByRole("heading", { name: "Your patterns" })).toBeInTheDocument();
    const mine = screen.getByRole("list", { name: "Your patterns" });
    expect(within(mine).getByText(/codename/)).toBeInTheDocument();
    // An answers-only pattern does not screen requests.
    expect(within(mine).queryByText(/only-answers/)).toBeNull();
  });

  it("Answers: switching to Block and saving PUTs the whole blob with only answers.action changed", async () => {
    await open(/^answers/i);
    expect(await screen.findByRole("switch", { name: "Inspect answers" })).toBeChecked();
    expect(screen.getByText("Deliver the answer and record it.")).toBeInTheDocument();
    expect(screen.getByText("Withhold it. A streamed answer stops at the match; what was already sent stays sent.")).toBeInTheDocument();
    // The bundled answer patterns are listed, the request ones are not.
    expect(screen.getByText(/answer_private_key/)).toBeInTheDocument();
    expect(screen.queryByText(/ignore_prev/)).toBeNull();
    await userEvent.click(screen.getByRole("radio", { name: /^Block/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save answer settings" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toEqual({ ...SEEDED, answers: { enabled: true, action: "block" } });
  });

  it("Custom patterns: a table of id, direction, description and pattern", async () => {
    await open(/^custom patterns/i);
    const table = await screen.findByRole("table", { name: "Custom patterns" });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["ID", "Applies to", "Description", "Pattern", ""]);
    const row = within(table).getByText("codename").closest("tr")!;
    expect(within(row).getByText("Requests and answers")).toBeInTheDocument();
    expect(within(row).getByText("An internal codename")).toBeInTheDocument();
    expect(within(row).getByText("project\\s+x")).toHaveClass("mono");
    expect(screen.getByText("2 of 50")).toBeInTheDocument();
  });

  it("Custom patterns: adding one through the dialog PUTs it appended, the rest unchanged", async () => {
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Add pattern" }));
    const dialog = await screen.findByRole("dialog", { name: "New custom pattern" });
    expect(within(dialog).getByText("Lowercase letters, digits, - and _.")).toBeInTheDocument();
    expect(within(dialog).getByText("A regular expression in Go RE2 syntax, up to 512 characters.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Add" })).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText("ID"), "secret-word");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "abracadabra");
    await userEvent.click(within(dialog).getByRole("combobox", { name: "Direction" }));
    await userEvent.click(screen.getByRole("option", { name: "Answers" }));
    await userEvent.type(within(dialog).getByLabelText("Description"), "A magic word");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toEqual({
      ...SEEDED,
      custom_patterns: [...SEEDED.custom_patterns, { id: "secret-word", pattern: "abracadabra", direction: "answer", description: "A magic word" }],
    });
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "New custom pattern" })).toBeNull());
  });

  it("Custom patterns: a duplicate id is named under ID once the field was left, before submit", async () => {
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Add pattern" }));
    const dialog = await screen.findByRole("dialog", { name: "New custom pattern" });
    const id = within(dialog).getByLabelText("ID");
    await userEvent.type(id, "codename");
    // Nothing is said while the field is being typed in.
    expect(within(dialog).queryByRole("alert")).toBeNull();
    await userEvent.tab();
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("A pattern with this id already exists.");
    expect(within(dialog).getByRole("button", { name: "Add" })).toBeDisabled();
    expect(puts).toHaveLength(0);
  });

  it("Custom patterns: an id with a capital letter or a space is refused on blur", async () => {
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Add pattern" }));
    const dialog = await screen.findByRole("dialog", { name: "New custom pattern" });
    await userEvent.type(within(dialog).getByLabelText("ID"), "Not Valid");
    await userEvent.tab();
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Lowercase letters, digits, - and _");
  });

  it("Custom patterns: the server's 400 about the pattern shows under Pattern", async () => {
    server.use(http.put("/api/v1/guardrails/settings", () => HttpResponse.json(
      { error: 'custom pattern "bad": not a valid regular expression (Go RE2 syntax): missing closing ): `(`' }, { status: 400 },
    )));
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Add pattern" }));
    const dialog = await screen.findByRole("dialog", { name: "New custom pattern" });
    await userEvent.type(within(dialog).getByLabelText("ID"), "bad");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "(");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent("not a valid regular expression");
    expect(within(dialog).getByLabelText("Pattern")).toHaveAccessibleDescription(/not a valid regular expression/);
    expect(screen.getByRole("dialog", { name: "New custom pattern" })).toBeInTheDocument();
  });

  it("Custom patterns: a 400 that names no field shows as an alert in the dialog", async () => {
    server.use(http.put("/api/v1/guardrails/settings", () => HttpResponse.json(
      { error: "at most 50 custom patterns are allowed, got 51" }, { status: 400 },
    )));
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Add pattern" }));
    const dialog = await screen.findByRole("dialog", { name: "New custom pattern" });
    await userEvent.type(within(dialog).getByLabelText("ID"), "one-more");
    await userEvent.type(within(dialog).getByLabelText("Pattern"), "abc");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("at most 50 custom patterns");
  });

  it("Custom patterns: editing a pattern PUTs it in place", async () => {
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Edit pattern codename" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit codename" });
    expect(within(dialog).getByLabelText("ID")).toBeDisabled();
    const desc = within(dialog).getByLabelText("Description");
    await userEvent.clear(desc);
    await userEvent.type(desc, "Renamed");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0].custom_patterns).toEqual([{ ...SEEDED.custom_patterns[0], description: "Renamed" }, SEEDED.custom_patterns[1]]);
  });

  it("Custom patterns: deleting asks once with the id named, then PUTs without it", async () => {
    await open(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Delete pattern codename" }));
    const confirm = await screen.findByRole("dialog", { name: "Delete pattern?" });
    expect(within(confirm).getByText("codename")).toBeInTheDocument();
    expect(puts).toHaveLength(0);
    await userEvent.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toEqual({ ...SEEDED, custom_patterns: [SEEDED.custom_patterns[1]] });
  });

  it("Custom patterns: Add is disabled at 50, with a line that says why", async () => {
    db.guardrailSettings.global.custom_patterns = Array.from({ length: 50 }, (_, i) => ({
      id: `p${i}`, pattern: `word${i}`, direction: "request" as const, description: "",
    }));
    await open(/^custom patterns/i);
    expect(await screen.findByText("50 of 50")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add pattern" })).toBeDisabled();
    expect(screen.getByText("At most 50 patterns. Delete one to add another.")).toBeInTheDocument();
  });

  it("Judge: the Model select lists the models and saving PUTs judge.model", async () => {
    await open(/^judge/i);
    expect(await screen.findByText("Each judge call is a request of that model and costs like one.")).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("switch", { name: "Ask a model" }));
    await userEvent.click(screen.getByRole("combobox", { name: "Model" }));
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["burrow-simple", "burrow-intelligence"]);
    await userEvent.click(screen.getByRole("option", { name: "burrow-simple" }));
    await userEvent.click(screen.getByRole("radio", { name: /^Requests and answers/ }));
    expect(screen.getByText("Streamed answers are not judged.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: /^Refuse the request/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save judge settings" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toEqual({
      ...SEEDED,
      judge: { enabled: true, model: "burrow-simple", policy: "No legal advice.", check: "requests_and_answers", timeout_ms: 3000, on_failure: "refuse" },
    });
  });

  it("Judge: the policy has a counter up to 4000, and the help for each choice is there", async () => {
    await open(/^judge/i);
    expect(await screen.findByText("16 of 4000")).toBeInTheDocument();
    expect(screen.getByText("Keeps apps running while the judge is down; each case is recorded.")).toBeInTheDocument();
    expect(screen.getByText("Nothing unjudged gets through.")).toBeInTheDocument();
    expect(screen.getByText("Default 3000.")).toBeInTheDocument();
  });

  it("Judge: a time limit outside 500 to 30000 is refused on blur, and Save is off", async () => {
    await open(/^judge/i);
    const limit = await screen.findByLabelText("Time limit (ms)");
    await userEvent.clear(limit);
    await userEvent.type(limit, "100");
    expect(screen.queryByText(/500 to 30000/)).toBeNull();
    await userEvent.tab();
    expect(await screen.findByRole("alert")).toHaveTextContent("Use a number from 500 to 30000.");
    expect(screen.getByRole("button", { name: "Save judge settings" })).toBeDisabled();
    await userEvent.clear(limit);
    await userEvent.type(limit, "30000");
    await userEvent.tab();
    expect(screen.queryByText(/500 to 30000\./)).toBeNull();
    expect(screen.getByRole("button", { name: "Save judge settings" })).toBeEnabled();
  });

  it("Judge: with the switch on and no model chosen, Save is off and says what is missing", async () => {
    await open(/^judge/i);
    await userEvent.click(await screen.findByRole("switch", { name: "Ask a model" }));
    expect(screen.getByRole("button", { name: "Save judge settings" })).toBeDisabled();
    expect(screen.getByText("Choose a model.")).toBeInTheDocument();
  });

  it("Judge: with no models, the section links to creating one", async () => {
    db.aiModels.length = 0;
    await open(/^judge/i);
    const link = await screen.findByRole("link", { name: /create a model/i });
    expect(link).toHaveAttribute("href", "/gateway/models?new=1");
    expect(screen.queryByRole("combobox", { name: "Model" })).toBeNull();
  });

  it("saving one section keeps what another section changed since: the blob is read from the stored settings", async () => {
    await open(/^answers/i);
    await userEvent.click(await screen.findByRole("radio", { name: /^Block/ }));
    await openMore(/^custom patterns/i);
    await userEvent.click(await screen.findByRole("button", { name: "Delete pattern codename" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Delete pattern?" })).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(puts).toHaveLength(1));
    await userEvent.click(screen.getByRole("button", { name: "Save answer settings" }));
    await waitFor(() => expect(puts).toHaveLength(2));
    expect(puts[1].custom_patterns).toEqual([SEEDED.custom_patterns[1]]);
    expect(puts[1].answers.action).toBe("block");
  });

  describe("without ai:configure:any", () => {
    beforeEach(() => { db.me.permissions = ALL_PERMISSIONS.filter((p) => p !== "ai:configure:any"); });

    it("shows the values and offers no Save, Add, Edit or Delete", async () => {
      mount();
      for (const name of [/^requests/i, /^answers/i, /^custom patterns/i, /^judge/i]) {
        await userEvent.click(await screen.findByRole("button", { name }));
      }
      // Wait until the permissions are known: the values are in the document either way.
      const table = await screen.findByRole("table", { name: "Custom patterns" });
      expect(within(table).getByText("codename")).toBeInTheDocument();
      expect(screen.getByRole("switch", { name: "Inspect answers" })).toBeChecked();
      expect(screen.getByLabelText("Policy")).toHaveValue("No legal advice.");
      // The permissions arrive with /me; give them time, then nothing may have appeared.
      await new Promise((r) => setTimeout(r, 150));
      expect(screen.queryByRole("button", { name: "Add pattern" })).toBeNull();
      expect(screen.queryByRole("button", { name: /^save (guardrails|answer|judge)/i })).toBeNull();
      expect(screen.queryByRole("button", { name: /^(edit|delete) pattern/i })).toBeNull();
      expect(screen.getByRole("switch", { name: "Inspect answers" })).toBeDisabled();
    });
  });
});
