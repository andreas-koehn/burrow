import { describe, it, expect, afterEach, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import { Link, Routes, Route, useLocation } from "react-router-dom";
import Clients from "@/pages/Clients";
import { Layout } from "@/components/Layout";

function LocationProbe() {
  const { pathname, search } = useLocation();
  return <div data-testid="location">{pathname + search}</div>;
}
const currentLocation = () => screen.getByTestId("location").textContent ?? "";
const mountAt = (route: string) => renderApp(<><Clients /><LocationProbe /></>, route);
const mount = () => mountAt("/clients");

const table = () => screen.getByRole("table", { name: "Clients" });
/** The cell of a row under the column of that name. */
const cell = (tr: HTMLElement, column: string) => {
  const at = within(table()).getAllByRole("columnheader").findIndex((h) => h.textContent === column);
  return within(tr).getAllByRole("cell")[at]!;
};
const rowOf = async (name: string) => (await within(await screen.findByRole("table", { name: "Clients" })).findByText(name)).closest("tr")!;
/** The client names in table order: the name is the first thing in its cell, above the address or the sessions. */
const names = () => within(table()).getAllByRole("row").slice(1).map((tr) => within(tr).getAllByRole("cell")[0]!.firstChild?.textContent);

describe("Clients overview", () => {
  it("lists connected clients with their platform", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    expect(await screen.findByText("office-box-1")).toBeInTheDocument();
    const row = screen.getByText("office-box-1").closest("tr")!;
    expect(within(row).getByText(/linux/i)).toBeInTheDocument();
  });

  it("writes the status of a live client out as Online", async () => {
    mount();
    const row = (await screen.findByText("office-box-1")).closest("tr")!;
    expect(within(row).getByText("Online")).toHaveClass("badge", "status-online");
    expect(within(row).queryByText("connected")).toBeNull();
  });

  it("has a link to client detail", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    const row = (await screen.findByText("office-box-1")).closest("tr")!;
    // The "View" button link — exact text match to avoid collision with the services-count link
    const links = within(row).getAllByRole("link");
    const viewLink = links.find((l) => l.textContent?.trim() === "View");
    expect(viewLink).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });

  it("has one row per client, with these columns", async () => {
    mount();
    await rowOf("office-box-1");
    expect(within(table()).getAllByRole("columnheader").map((h) => h.textContent))
      .toEqual(["Client", "Status", "Last seen", "Platform", "Version", "Serves", ""]);
    // The relay's order: online, then offline, then never connected.
    expect(names()).toEqual(["office-box-1", "koehn-laptop", "build-server"]);
  });
});

describe("Clients — every client the relay knows", () => {
  it("lists an offline client with what it served and when", async () => {
    mount();
    const row = (await screen.findByText("koehn-laptop")).closest("tr")!;
    expect(within(row).getByText("Offline")).toBeInTheDocument();
    expect(within(row).getByText(/ollama/)).toBeInTheDocument();
    expect(within(row).queryByRole("link", { name: "View" })).toBeNull();
    expect(cell(row, "Platform")).toHaveTextContent("darwin · arm64");
    expect(cell(row, "Version")).toHaveTextContent("0.6.0");
    // Relative for the eye, the exact UTC time on hover.
    const seen = within(cell(row, "Last seen")).getByText(/ago$|^last |^yesterday$/);
    expect(seen).toHaveAttribute("title", "16 May 2026, 17:30:00 UTC");
    // The address it connected from last, under its name.
    expect(within(cell(row, "Client")).getByText("198.51.100.23:40112")).toHaveClass("muted", "mono");
  });

  it("lists a client that never connected without platform or version", async () => {
    mount();
    const row = await rowOf("build-server");
    expect(within(row).getByText("Never connected")).toHaveClass("badge", "status-never");
    for (const column of ["Last seen", "Platform", "Version", "Serves"]) expect(cell(row, column), column).toHaveTextContent(/^—$/);
    expect(cell(row, "Client")).toHaveTextContent(/^build-server$/);
    expect(within(row).queryByRole("link")).toBeNull();
    expect(within(row).queryByText("update available")).toBeNull();
  });

  it("an online client links to its session", async () => {
    mount();
    const row = await rowOf("office-box-1");
    expect(within(row).getByRole("link", { name: "View" })).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
    expect(cell(row, "Last seen")).toHaveTextContent(/^now$/);
    // One session needs no list of sessions: its address stands under the name.
    expect(within(row).queryByRole("button")).toBeNull();
    expect(within(cell(row, "Client")).getByText("203.0.113.7:51234")).toHaveClass("muted", "mono");
  });

  it("two sessions of one token are one row that says so", async () => {
    db.clients.unshift({
      ...db.clients[0]!, session_id: "sess_second", remote_addr: "198.51.100.9:40000", service_count: 1,
      services: [{ ...db.clients[0]!.services[0]!, id: "tnl_metrics", name: "metrics" }],
    });
    mount();
    const row = await rowOf("office-box-1");
    expect(names().filter((n) => n?.startsWith("office-box-1"))).toHaveLength(1);
    const toggle = within(row).getByRole("button", { name: /2 sessions/ });
    expect(toggle).toHaveTextContent(/^2 sessions$/);
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    // Each session has its own address: none stands under the name, they are in the list.
    expect(within(row).queryByText("198.51.100.9:40000")).toBeNull();
    expect(within(row).queryByText("203.0.113.7:51234")).toBeNull();
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const list = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    const items = within(list).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual([
      expect.stringMatching(/sess_second.*198\.51\.100\.9:40000/),
      expect.stringMatching(/sess_4f7a9c0b2e81.*203\.0\.113\.7:51234/),
    ]);
    expect(within(items[1]!).getByRole("link")).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
    // "View" opens the newest session; the row names what all of them serve.
    expect(within(row).getByRole("link", { name: "View" })).toHaveAttribute("href", "/clients/sess_second");
    expect(cell(row, "Serves")).toHaveTextContent("metrics, ollama, web-staging");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(within(row).queryByText("198.51.100.9:40000")).toBeNull();
  });

  it("a served name that is a provider links to the provider", async () => {
    mount();
    const online = await rowOf("office-box-1");
    // ollama backs the provider of that name: the link leads there, not to the service behind it.
    expect(await within(online).findByRole("link", { name: "ollama" })).toHaveAttribute("href", "/gateway/providers/ollama");
    // No service is called web-staging: the name stays text.
    expect(cell(online, "Serves")).toHaveTextContent("ollama, web-staging");
    expect(within(online).queryByRole("link", { name: "web-staging" })).toBeNull();
    // grafana backs no provider: it links to the service.
    const offline = await rowOf("koehn-laptop");
    expect(within(offline).getByRole("link", { name: "grafana" })).toHaveAttribute("href", "/services/svc_graf01");
    expect(within(offline).getByRole("link", { name: "ollama" })).toHaveAttribute("href", "/gateway/providers/ollama");
  });

  it("a name that two services share is not linked to either", async () => {
    db.services.push({ ...db.services.find((s) => s.id === "svc_graf01")!, id: "svc_graf02", slug: "gf2222" });
    const { qc } = mount();
    const offline = await rowOf("koehn-laptop");
    await within(offline).findByRole("link", { name: "ollama" });
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(within(offline).queryByRole("link", { name: "grafana" })).toBeNull();
    expect(cell(offline, "Serves")).toHaveTextContent("grafana, ollama");
  });

  it("names what a client serves even when the services cannot be read", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    const { qc } = mount();
    const row = await rowOf("koehn-laptop");
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(cell(row, "Serves")).toHaveTextContent("grafana, ollama");
    expect(within(cell(row, "Serves")).queryByRole("link")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("the empty state offers to connect a client", async () => {
    db.tokens = []; db.clients = []; db.clientLastSessions = {};
    mount();
    const panel = await screen.findByRole("tabpanel", { name: "Clients" });
    expect(await within(panel).findByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect");
    expect(within(panel).getByRole("link", { name: "Connect a client" })).toHaveClass("btn");
    // The three lines of the Connect page.
    const lines = within(within(panel).getByRole("list", { name: "Commands" })).getAllByRole("listitem");
    expect(lines.map((li) => li.querySelector(".install-line-label")?.textContent)).toEqual(["1. Install", "2. Sign in", "3. Run"]);
    expect(screen.queryByRole("table", { name: "Clients" })).toBeNull();
    expect(screen.queryByRole("searchbox", { name: "Search clients" })).toBeNull();
  });

  it("falls back to live sessions on an older relay", async () => {
    server.use(http.get("/api/v1/clients/known", () => new HttpResponse(null, { status: 404 })));
    const { qc } = mount();
    const row = await rowOf("office-box-1");
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(names()).toEqual(["office-box-1"]);
    expect(within(row).getByText("Online")).toHaveClass("badge", "status-online");
    expect(cell(row, "Last seen")).toHaveTextContent(/^now$/);
    expect(cell(row, "Platform")).toHaveTextContent("linux · amd64");
    expect(within(row).getByRole("link", { name: "View" })).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
    // Such a relay does not say what a client serves, only how much.
    expect(within(cell(row, "Serves")).getByRole("link", { name: "2 services" })).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("an older relay without a connected client shows the empty state", async () => {
    db.clients = [];
    server.use(http.get("/api/v1/clients/known", () => new HttpResponse(null, { status: 404 })));
    mount();
    const panel = await screen.findByRole("tabpanel", { name: "Clients" });
    expect(await within(panel).findByRole("link", { name: "Connect a client" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("says so when the relay fails for another reason, without asking for the live sessions", async () => {
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    server.use(http.get("/api/v1/clients/known", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    try {
      mount();
      expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load clients: boom");
      expect(asked).not.toContain("/api/v1/clients");
    } finally {
      server.events.removeAllListeners();
    }
  });

  it("search matches the client name and what it serves", async () => {
    mount();
    await rowOf("office-box-1");
    const search = screen.getByRole("searchbox", { name: "Search clients" });
    await userEvent.type(search, "LAPTOP");
    expect(names()).toEqual(["koehn-laptop"]);
    await userEvent.clear(search);
    await userEvent.type(search, "graf");
    expect(names()).toEqual(["koehn-laptop"]);
    await userEvent.clear(search);
    await userEvent.type(search, "ollama");
    expect(names()).toEqual(["office-box-1", "koehn-laptop"]);
    await userEvent.clear(search);
    await userEvent.type(search, "zzz");
    expect(screen.queryByRole("table", { name: "Clients" })).toBeNull();
    expect(screen.getByText("No client matches “zzz”.")).toBeInTheDocument();
    // Nothing found is not the same as nothing there.
    expect(within(screen.getByRole("tabpanel", { name: "Clients" })).queryByRole("link", { name: "Connect a client" })).toBeNull();
    expect(search).toHaveValue("zzz");
  });
});

// The sidebar count and the page read the same rows, so they cannot disagree.
describe("Clients — the sidebar counts what the page lists", () => {
  const mountInShell = () => renderApp(
    <Routes><Route element={<Layout />}><Route path="/clients" element={<Clients />} /></Route></Routes>,
    "/clients",
  );
  const onlineRows = () => within(table()).getAllByRole("row").slice(1).filter((tr) => within(tr).queryByText("Online"));
  const sidebarCount = (n: number) => screen.findByRole("link", { name: `Clients, ${n} online` });

  it("the seed: three clients, one of them online", async () => {
    mountInShell();
    await rowOf("build-server");
    expect(onlineRows()).toHaveLength(1);
    expect(await sidebarCount(1)).toHaveAttribute("aria-current", "page");
  });

  it("two online tokens of one user with the same name are two rows, and two in the sidebar", async () => {
    const session = (session_id: string) => ({ ...db.clients[0]!, services: undefined, session_id, token_name: "laptop" });
    const row = (token_id: string, session_id: string) => ({
      token_id, token_name: "laptop", user_id: db.me.id, status: "online", last_seen: "2026-05-18T09:00:00Z",
      os: "linux", arch: "amd64", client_version: "0.6.0", remote_addr: "203.0.113.7:51234", served: [],
      sessions: [session(session_id)],
    });
    server.use(http.get("/api/v1/clients/known", () => HttpResponse.json([row("tok_a", "sess_a"), row("tok_b", "sess_b")])));
    mountInShell();
    await waitFor(() => expect(names()).toEqual(["laptop", "laptop"]));
    expect(onlineRows()).toHaveLength(2);
    expect(await sidebarCount(2)).toBeInTheDocument();
  });

  it("two sessions of one token are one row, and one in the sidebar", async () => {
    db.clients.unshift({ ...db.clients[0]!, session_id: "sess_second" });
    mountInShell();
    await rowOf("office-box-1");
    expect(onlineRows()).toHaveLength(1);
    expect(await sidebarCount(1)).toBeInTheDocument();
  });

  it("on an older relay every live session is a row, and the sidebar counts those rows", async () => {
    db.clients.unshift({ ...db.clients[0]!, session_id: "sess_second" });
    server.use(http.get("/api/v1/clients/known", () => new HttpResponse(null, { status: 404 })));
    mountInShell();
    await waitFor(() => expect(names()).toEqual(["office-box-1", "office-box-1"]));
    expect(onlineRows()).toHaveLength(2);
    expect(await sidebarCount(2)).toBeInTheDocument();
  });

  // The shell refreshes the list 30 seconds after its last answer; the page shows
  // the new rows and asks for nothing beside it.
  it("the page follows the shell's refresh, one request each time", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    const known = () => asked.filter((p) => p === "/api/v1/clients/known").length;
    const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
    try {
      // The reader opens another page first, and Clients ten seconds later.
      renderApp(
        <Routes>
          <Route element={<Layout />}>
            <Route path="/start" element={<Link to="/clients">open clients</Link>} />
            <Route path="/clients" element={<Clients />} />
          </Route>
        </Routes>,
        "/start",
      );
      await sidebarCount(1);
      expect(known()).toBe(1);
      await advance(10_000);
      fireEvent.click(screen.getByRole("link", { name: "open clients" }));
      await rowOf("office-box-1");
      await waitFor(() => expect(known()).toBe(2)); // opening the page asks again
      // koehn-laptop comes back online.
      db.clients.push({ ...db.clients[0]!, session_id: "sess_laptop", token_name: "koehn-laptop" });
      await advance(25_000);
      expect(onlineRows()).toHaveLength(1);
      expect(known()).toBe(2);
      await advance(6_000);
      await waitFor(() => expect(onlineRows()).toHaveLength(2));
      expect(await sidebarCount(2)).toBeInTheDocument();
      expect(known()).toBe(3);
      await advance(25_000);
      expect(known()).toBe(3);
      await advance(6_000);
      await waitFor(() => expect(known()).toBe(4));
    } finally {
      server.events.removeAllListeners();
      vi.useRealTimers();
    }
  }, 15000);

  it("asks for the list once for both", async () => {
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      const { qc } = mountInShell();
      await rowOf("office-box-1");
      await sidebarCount(1);
      await waitFor(() => expect(qc.isFetching()).toBe(0));
      expect(asked.filter((p) => p === "/api/v1/clients/known")).toHaveLength(1);
      expect(asked).not.toContain("/api/v1/clients");
    } finally {
      server.events.removeAllListeners();
    }
  });
});

describe("Clients — version", () => {
  const row = async () => (await screen.findByText("office-box-1")).closest("tr")!;
  const versionCell = (tr: HTMLElement) => cell(tr, "Version");

  it("has a Version column with the client's version", async () => {
    mount();
    const tr = await row();
    expect(within(screen.getByRole("table", { name: "Clients" })).getByRole("columnheader", { name: "Version" })).toBeInTheDocument();
    expect(versionCell(tr)).toHaveTextContent("0.2.0");
  });

  it("marks a client older than the relay", async () => {
    // Seed: client 0.2.0, relay 0.6.0.
    mount();
    const tr = await row();
    const badge = await within(tr).findByText("update available");
    expect(badge.closest(".badge")).toHaveTextContent("update available (older than the relay)");
    expect(badge.closest(".badge")!.querySelector(".visually-hidden")).toHaveTextContent("(older than the relay)");
  });

  it.each([
    ["the same version", "0.6.0", "0.6.0"],
    ["the same version with a v", "v0.6.0", "0.6.0"],
    ["a newer client", "0.10.0", "0.9.9"],
    ["a client build without a version number", "develop", "0.6.0"],
    ["no client version", "", "0.6.0"],
    ["a relay build without a version number", "0.2.0", "develop"],
  ])("has no mark for %s", async (_label, clientVersion, relayVersion) => {
    db.clients[0]!.client_version = clientVersion;
    db.discovery.version = relayVersion;
    const { qc } = mount();
    const tr = await row();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(within(tr).queryByText("update available")).toBeNull();
    expect(versionCell(tr)).toHaveTextContent(clientVersion || "—");
  });

  it("compares number by number: 0.9.9 is older than 0.10.0", async () => {
    db.clients[0]!.client_version = "0.9.9";
    db.discovery.version = "v0.10.0";
    mount();
    expect(await within(await row()).findByText("update available")).toBeInTheDocument();
  });

  it("has no mark when the relay does not say its version", async () => {
    server.use(http.get("/api/v1/client/discovery", () => HttpResponse.json({ error: "not found" }, { status: 404 })));
    const { qc } = mount();
    const tr = await row();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(within(tr).queryByText("update available")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("Clients — Clients | Tokens tabs", () => {
  it("has Clients and Tokens tabs that follow the URL", async () => {
    mount();
    expect(await screen.findByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("table", { name: "Clients" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Tokens" }));
    expect(currentLocation()).toMatch(/[?&]tab=tokens/);
    expect(await screen.findByRole("table", { name: /tokens/i })).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "Clients" })).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: "Clients" }));
    expect(currentLocation()).toBe("/clients");
  });

  it("opens on the Tokens tab from the old link", async () => {
    mountAt("/clients?tab=tokens");
    expect(await screen.findByRole("tab", { name: "Tokens" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
  });

  it("treats ?tab=clients and an unknown tab as the clients list", async () => {
    const first = mountAt("/clients?tab=clients");
    expect(await screen.findByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
    first.unmount();
    mountAt("/clients?tab=nope");
    expect(await screen.findByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
  });

  it("names the other kinds of credential on the Tokens tab", async () => {
    mountAt("/clients?tab=tokens");
    expect(await screen.findByRole("link", { name: "gateway key" })).toHaveAttribute("href", "/gateway/keys");
    expect(screen.getByRole("link", { name: "service" })).toHaveAttribute("href", "/services");
    expect(screen.getByRole("link", { name: "automation token" })).toHaveAttribute("href", "/settings/automation");
  });

  it("keeps one page header, with Connect a client as its action, on both tabs", async () => {
    mountAt("/clients?tab=tokens");
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expect(screen.getByRole("heading", { level: 1, name: "Clients" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect");
  });

  it("keeps the Tokens tab usable when the clients list is refused", async () => {
    server.use(http.get("/api/v1/clients/known", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load clients/i);
    await userEvent.click(screen.getByRole("tab", { name: "Tokens" }));
    expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("ties each panel to its tab, and Home/End reach the first and last tab", async () => {
    mount();
    const clients = await screen.findByRole("tab", { name: "Clients" });
    expect(screen.getByRole("tabpanel", { name: "Clients" })).toHaveAttribute("id", clients.getAttribute("aria-controls")!);
    clients.focus();
    await userEvent.keyboard("{End}");
    const tokens = screen.getByRole("tab", { name: "Tokens" });
    expect(tokens).toHaveFocus();
    expect(tokens).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel", { name: "Tokens" })).toBeInTheDocument();
    await userEvent.keyboard("{Home}");
    expect(screen.getByRole("tab", { name: "Clients" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
  });

  it("its links that look like buttons are links only: one tab stop each", async () => {
    mount();
    const row = (await screen.findByText("office-box-1")).closest("tr")!;
    for (const link of [screen.getByRole("link", { name: "Connect a client" }), within(row).getByRole("link", { name: "View" })]) {
      expect(link.querySelector("button")).toBeNull();
      expect(link).toHaveClass("btn");
    }
  });

  // The clients list is admin only: anyone else starts on their own tokens.
  describe("for a non-admin", () => {
    afterEach(() => server.events.removeAllListeners());

    it("opens on the Tokens tab without asking for the clients list", async () => {
      db.me.role = "user";
      const asked: string[] = [];
      server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
      mount();
      expect(await screen.findByRole("tab", { name: "Tokens" })).toHaveAttribute("aria-selected", "true");
      expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
      expect(screen.queryByRole("alert")).toBeNull();
      expect(asked).not.toContain("/api/v1/clients/known");
      expect(asked).not.toContain("/api/v1/clients");
      expect(currentLocation()).toBe("/clients");
    });

    it("can still open the Clients tab, which the URL then names", async () => {
      db.me.role = "user";
      // The relay refuses the list to anyone but an admin; the mock does the same.
      const asked: string[] = [];
      server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
      mount();
      await userEvent.click(await screen.findByRole("tab", { name: "Clients" }));
      expect(currentLocation()).toBe("/clients?tab=clients");
      expect(screen.getByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
      expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load clients/i);
      // A refusal is not an older relay: the live sessions are not asked for instead.
      expect(asked).toContain("/api/v1/clients/known");
      expect(asked).not.toContain("/api/v1/clients");
      await userEvent.click(screen.getByRole("tab", { name: "Tokens" }));
      expect(currentLocation()).toBe("/clients");
    });
  });
});
