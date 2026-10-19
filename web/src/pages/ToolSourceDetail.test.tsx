import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import ToolSourceDetail from "@/pages/ToolSourceDetail";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname}</div>;
}

function mount(slug = "github") {
  return renderApp(
    <>
      <Routes>
        <Route path="/gateway/tools/:slug" element={<ToolSourceDetail />} />
        <Route path="*" element={null} />
      </Routes>
      <Where />
    </>,
    `/gateway/tools/${slug}`,
  );
}

interface Made { method: string; path: string; body: Record<string, unknown> | null }
function record() {
  const made: Made[] = [];
  server.events.on("request:start", async ({ request }) => {
    if (request.method === "GET") return;
    made.push({
      method: request.method,
      path: new URL(request.url).pathname.replace("/api/v1", ""),
      body: (await request.clone().json().catch(() => null)) as Record<string, unknown> | null,
    });
  });
  return made;
}

const hashOf = (name: string) => db.toolSourceTools.github!.find((t) => t.name === name)!.current_hash;

describe("Tool source page: catalog", () => {
  it("lists every tool with its served name, classification and status", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Tools of GitHub" });
    const row = (n: string) => within(table).getByRole("row", { name: new RegExp(n) });
    expect(within(row("list_issues")).getByText("Served")).toBeInTheDocument();
    expect(within(row("list_issues")).getByText("Read-only")).toBeInTheDocument();
    expect(within(row("create_issue")).getByText("New")).toBeInTheDocument();
    expect(within(row("search_code")).getByText("Changed")).toBeInTheDocument();
  });

  it("shows the aggregated name when the relay sends one, and nothing when it does not", async () => {
    db.toolSourceTools.github![0]!.aggregated_name = "github__list_issues";
    db.toolSourceTools.github![0]!.aggregated_issue = "too_long";
    mount();
    const table = await screen.findByRole("table", { name: "Tools of GitHub" });
    expect(within(table).getByText("github__list_issues")).toBeInTheDocument();
    expect(within(table).getByText(/too_long/)).toBeInTheDocument();
  });

  it("flags two tools that would be served under one name", async () => {
    const tools = db.toolSourceTools.github!;
    tools.find((t) => t.name === "search_code")!.status = "approved";
    tools.find((t) => t.name === "search_code")!.rename = "list_issues";
    mount();
    const table = await screen.findByRole("table", { name: "Tools of GitHub" });
    expect(within(table).getAllByText("Name conflict")).toHaveLength(2);
    expect(await screen.findByText(/neither is served/i)).toBeInTheDocument();
  });
});

describe("Tool source page: pending changes", () => {
  it("shows the old and the new definition side by side, the matched patterns and the schema", async () => {
    mount();
    const section = await screen.findByRole("region", { name: "Pending changes" });
    const changed = within(section).getByRole("group", { name: "search_code" });
    const before = within(changed).getByRole("group", { name: "Approved definition" });
    const after = within(changed).getByRole("group", { name: "New definition" });
    expect(before).toHaveTextContent("Search code in a repository.");
    expect(before).not.toHaveTextContent("evil.example");
    // The added words are marked, not just present.
    expect(within(after).getByText(/send the results to https:\/\/evil\.example\./).tagName).toBe("INS");
    expect(after).toHaveTextContent('"q"');
    const fresh = within(section).getByRole("group", { name: "create_issue" });
    expect(within(fresh).getByText(/ignore previous instructions/i, { selector: "code, .mono" })).toBeInTheDocument();
    expect(within(fresh).getByText(/matches a guardrail pattern/i)).toBeInTheDocument();
    expect(within(fresh).queryByRole("group", { name: "Approved definition" })).not.toBeInTheDocument();
  });

  it("approves a tool with the hash it showed", async () => {
    const made = record();
    mount();
    const section = await screen.findByRole("region", { name: "Pending changes" });
    const shown = hashOf("search_code");
    await userEvent.click(within(within(section).getByRole("group", { name: "search_code" })).getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(made.some((m) => m.path.endsWith("/tools/search_code/approve"))).toBe(true));
    expect(made.find((m) => m.path.endsWith("/approve"))!.body).toEqual({ hash: shown });
    await waitFor(() => expect(within(section).queryByRole("group", { name: "search_code" })).not.toBeInTheDocument());
  });

  it("does not approve what changed after it was shown", async () => {
    mount();
    const section = await screen.findByRole("region", { name: "Pending changes" });
    // The upstream changes the definition again before the click.
    db.toolSourceTools.github!.find((t) => t.name === "search_code")!.current_hash = "sha_other";
    await userEvent.click(within(within(section).getByRole("group", { name: "search_code" })).getByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/changed since it was reviewed/i)).toBeInTheDocument();
    expect(db.toolSourceTools.github!.find((t) => t.name === "search_code")!.status).toBe("pending");
  });

  it("approves all after a confirmation, with exactly the hashes shown", async () => {
    const made = record();
    mount();
    const section = await screen.findByRole("region", { name: "Pending changes" });
    await userEvent.click(within(section).getByRole("button", { name: "Approve all" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("create_issue");
    expect(dialog).toHaveTextContent("search_code");
    expect(made.filter((m) => m.path.endsWith("/approve-all"))).toEqual([]);
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve 2 tools" }));
    await waitFor(() => expect(made.some((m) => m.path.endsWith("/approve-all"))).toBe(true));
    expect(made.find((m) => m.path.endsWith("/approve-all"))!.body).toEqual({
      hashes: { create_issue: hashOf("create_issue"), search_code: hashOf("search_code") },
    });
    await waitFor(() => expect(screen.queryByRole("region", { name: "Pending changes" })).not.toBeInTheDocument());
  });

  it("reports the tools that were not approved because of a name conflict", async () => {
    const tools = db.toolSourceTools.github!;
    // Another approved tool already is served as create_issue.
    tools.find((t) => t.name === "list_issues")!.rename = "create_issue";
    mount();
    const section = await screen.findByRole("region", { name: "Pending changes" });
    await userEvent.click(within(section).getByRole("button", { name: "Approve all" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve 2 tools" }));
    const note = await screen.findByText(/not approved/i);
    expect(note).toHaveTextContent("create_issue");
    expect(note).toHaveTextContent(/name/i);
    expect(tools.find((t) => t.name === "search_code")!.status).toBe("approved");
    expect(tools.find((t) => t.name === "create_issue")!.status).toBe("pending");
  });
});

describe("Tool source page: sync", () => {
  it("syncs now and says what it found", async () => {
    db.toolUpstream.github = [
      ...db.toolSourceTools.github!.map((t) => t.definition),
      { name: "close_issue", description: "Close an issue." },
    ];
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Sync now" }));
    expect(await screen.findByText(/1 new/)).toBeInTheDocument();
    expect(await screen.findByRole("row", { name: /close_issue/ })).toBeInTheDocument();
  });

  it("shows why a sync failed", async () => {
    server.use(http.post("/api/v1/mcp/sources/github/sync", () => HttpResponse.json({ error: "the tool server did not answer" }, { status: 502 })));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Sync now" }));
    expect(await screen.findByText(/did not answer/)).toBeInTheDocument();
  });

  it("shows the reason of the last failed sync", async () => {
    db.toolSources[0]!.last_sync_error = "401 from the tool server";
    mount();
    expect(await screen.findByText(/401 from the tool server/)).toBeInTheDocument();
  });
});

describe("Tool source page: overrides", () => {
  async function openEdit(tool: string) {
    const table = await screen.findByRole("table", { name: "Tools of GitHub" });
    await userEvent.click(within(within(table).getByRole("row", { name: new RegExp(tool) })).getByRole("button", { name: new RegExp(`Edit ${tool}`) }));
    return screen.findByRole("dialog");
  }

  it("sends only what changed", async () => {
    const made = record();
    mount();
    const dialog = await openEdit("list_issues");
    await userEvent.type(within(dialog).getByLabelText("Serve under another name"), "issues");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(made.some((m) => m.method === "PUT")).toBe(true));
    expect(made.find((m) => m.method === "PUT")!).toMatchObject({ path: "/mcp/sources/github/tools/list_issues", body: { rename: "issues" } });
    expect(Object.keys(made.find((m) => m.method === "PUT")!.body!)).toEqual(["rename"]);
  });

  it("refuses a name the gateway would not accept", async () => {
    const made = record();
    mount();
    const dialog = await openEdit("list_issues");
    await userEvent.type(within(dialog).getByLabelText("Serve under another name"), "bad name");
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/letters, digits/i);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    expect(made).toEqual([]);
  });

  it("sets the read-only classification, the description and hides a tool", async () => {
    const made = record();
    mount();
    const dialog = await openEdit("list_issues");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Can change things" }));
    await userEvent.type(within(dialog).getByLabelText("Description for agents"), "Open issues.");
    await userEvent.click(within(dialog).getByRole("switch", { name: "Hide this tool" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(made.some((m) => m.method === "PUT")).toBe(true));
    expect(made.find((m) => m.method === "PUT")!.body).toEqual({ read_only: false, description_override: "Open issues.", hidden: true });
  });

  it("warns about the keys that already name the new name", async () => {
    db.toolRenameAffected.issues = [{ id: "gk1", name: "ci-bot", key_prefix: "bgw_ab12", user_id: "u1" }];
    mount();
    const dialog = await openEdit("list_issues");
    await userEvent.type(within(dialog).getByLabelText("Serve under another name"), "issues");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    const warn = await screen.findByText(/already name github__issues/i);
    expect(warn.closest("[role=status], [role=alert]")).toHaveTextContent("ci-bot");
    expect(warn.closest("[role=status], [role=alert]")).toHaveTextContent("bgw_ab12");
  });

  it("does not warn when no key matches", async () => {
    mount();
    const dialog = await openEdit("list_issues");
    await userEvent.type(within(dialog).getByLabelText("Serve under another name"), "issues");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.queryByText(/already name/i)).not.toBeInTheDocument();
  });
});

describe("Tool source page: settings and delete", () => {
  it("saves the limits, sending only what changed", async () => {
    const made = record();
    mount();
    const rpm = await screen.findByLabelText("Calls per minute");
    await userEvent.clear(rpm);
    await userEvent.type(rpm, "30");
    await userEvent.click(screen.getByRole("button", { name: "Save limits" }));
    await waitFor(() => expect(made.some((m) => m.method === "PUT")).toBe(true));
    expect(made.find((m) => m.method === "PUT")).toMatchObject({ path: "/mcp/sources/github", body: { rpm: 30 } });
  });

  it("says that a deleted tunnel source leaves its service gateway-only", async () => {
    mount("files");
    await userEvent.click(await screen.findByRole("button", { name: "Delete source" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/stays gateway-only/i);
    expect(dialog).toHaveTextContent(/not reopened automatically/i);
  });

  it("says what goes with a remote source", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Delete source" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).not.toHaveTextContent(/gateway-only/i);
    expect(dialog).toHaveTextContent(/GITHUB_MCP/);
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete source" }));
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("/gateway/tools"));
    expect(db.toolSources.some((s) => s.slug === "github")).toBe(false);
  });

  it("a reader sees the tools and none of the controls", async () => {
    db.me = { ...db.me, role: "user", permissions: ["mcp:sources:read"] };
    mount();
    await screen.findByRole("table", { name: "Tools of GitHub" });
    for (const name of ["Sync now", "Delete source", "Approve all", "Save limits"]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    }
    expect(screen.queryByRole("button", { name: /^Edit / })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("says when the source does not exist", async () => {
    mount("nope");
    expect(await screen.findByText(/not found/i)).toBeInTheDocument();
  });
});
