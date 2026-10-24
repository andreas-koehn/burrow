import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Routes, Route } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import ConnectClient from "@/pages/ConnectClient";
import { Layout } from "@/components/Layout";
import type { ClientView } from "@/lib/contract";
import { USER_PERMISSIONS } from "@/auth/permissions";

function mount(route = "/clients/connect") {
  return renderApp(
    <Routes><Route path="/clients/connect" element={<ConnectClient />} /></Routes>,
    route,
  );
}

const otherWays = () => screen.getByRole("button", { name: "Other ways to connect" });

// The downloads and the burrow.yaml example live behind "Other ways to connect".
async function mountOther() {
  const r = mount();
  await userEvent.click(otherWays());
  return r;
}

const client = (session_id: string, token_name: string): ClientView => ({
  session_id, token_name, user_id: "u", remote_addr: "1.2.3.4:5000", os: "linux", arch: "amd64",
  client_version: "0.6.0", service_count: 0, total_bytes_in: 0, total_bytes_out: 0,
});

// Serves GET /clients from a list the test changes, and counts the requests.
function clientsFeed(initial: ClientView[]) {
  const feed = { list: initial, asked: 0 };
  server.use(http.get("/api/v1/clients", () => { feed.asked++; return HttpResponse.json(feed.list); }));
  return feed;
}

const pause = (ms: number) => new Promise((r) => setTimeout(r, ms));
const lineTexts = () => Array.from(document.querySelectorAll(".install-line pre code")).map((c) => c.textContent);

describe("Connect a client — three lines", () => {
  it("shows the three lines first, with the relay address from window.location", () => {
    mount();
    expect(lineTexts()).toEqual([
      `curl -fsSL ${window.location.origin}/install.sh | sh`,
      `burrow login ${window.location.origin}`,
      "burrow http 3000",
    ]);
    const lines = document.querySelector(".install-lines")!;
    expect(lines.compareDocumentPosition(otherWays()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    for (const name of ["Copy install command", "Copy sign-in command", "Copy run command"]) {
      expect(screen.getByRole("button", { name })).toBeInTheDocument();
    }
  });

  it("points to the Tokens tab for signing in with a token instead", () => {
    mount();
    expect(screen.getByRole("link", { name: "Clients, tab Tokens" })).toHaveAttribute("href", "/clients?tab=tokens");
  });

  it("waits with an accessible status; a client that was there when the page opened does not count", async () => {
    const feed = clientsFeed([client("sess_old", "old-box")]);
    mount();
    const waiting = await screen.findByText("Waiting for your client…");
    const status = waiting.closest("[role=status]")!;
    expect(status).not.toBeNull();
    expect(status.querySelector(".spinner")).toHaveAttribute("aria-hidden", "true");
    // Two answers, both with the same client: still waiting.
    await waitFor(() => expect(feed.asked).toBeGreaterThanOrEqual(2), { timeout: 6000 });
    expect(screen.getByText("Waiting for your client…")).toBeInTheDocument();
    expect(screen.queryByText(/^Connected:/)).toBeNull();
  }, 15000);

  it("switches to Connected with a link to the client once a new one appears, then stops asking", async () => {
    const feed = clientsFeed([client("sess_old", "old-box")]);
    mount();
    await screen.findByText("Waiting for your client…");
    await waitFor(() => expect(feed.asked).toBeGreaterThanOrEqual(1));
    feed.list = [client("sess_old", "old-box"), client("sess_new", "edge-99")];
    const link = await screen.findByRole("link", { name: "edge-99" }, { timeout: 6000 });
    expect(link).toHaveAttribute("href", "/clients/sess_new");
    const status = link.closest("[role=status]")!;
    expect(status.textContent).toMatch(/^Connected: edge-99/);
    expect(screen.queryByText("Waiting for your client…")).toBeNull();
    const asked = feed.asked;
    await pause(2500);
    expect(feed.asked).toBe(asked);
    // It stays, also when the list changes again.
    expect(screen.getByRole("link", { name: "edge-99" })).toBeInTheDocument();
  }, 15000);

  // The sidebar counts from the list of known clients, which this page does not poll.
  it("a client that arrives is counted in the sidebar at once, not at the shell's next refresh", async () => {
    renderApp(
      <Routes><Route element={<Layout />}><Route path="/clients/connect" element={<ConnectClient />} /></Route></Routes>,
      "/clients/connect",
    );
    await screen.findByText("Waiting for your client…");
    // Seed: one client is connected.
    expect(await screen.findByRole("link", { name: "Clients, 1 online" })).toBeInTheDocument();
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      db.tokens.push({ id: "tok_edge", name: "edge-99", last_used: null, created_at: "2026-05-18T10:00:00Z" });
      db.clients.push({ ...db.clients[0]!, session_id: "sess_new", token_name: "edge-99" });
      const link = await screen.findByRole("link", { name: "edge-99" }, { timeout: 6000 });
      expect(link).toHaveAttribute("href", "/clients/sess_new");
      expect(await screen.findByRole("link", { name: "Clients, 2 online" }, { timeout: 3000 })).toBeInTheDocument();
      // Told once, not with every answer of the poll.
      await pause(2500);
      expect(asked.filter((p) => p === "/api/v1/clients/known")).toHaveLength(1);
    } finally {
      server.events.removeAllListeners();
    }
  }, 20000);

  it("stops asking when the page is left", async () => {
    const feed = clientsFeed([]);
    const { unmount } = mount();
    await waitFor(() => expect(feed.asked).toBeGreaterThanOrEqual(2), { timeout: 6000 });
    unmount();
    const asked = feed.asked;
    await pause(2500);
    expect(feed.asked).toBe(asked);
  }, 15000);

  it("someone who may not list clients is told where the client will appear, and nothing is asked", async () => {
    db.me = { ...db.me, role: "user", permissions: [...USER_PERMISSIONS] };
    const feed = clientsFeed([]);
    mount();
    expect(await screen.findByText("Your client appears under Clients once it connects.")).toBeInTheDocument();
    await pause(300);
    expect(feed.asked).toBe(0);
    expect(screen.queryByText("Waiting for your client…")).toBeNull();
  });

  it("stops waiting when the clients list cannot be read", async () => {
    let asked = 0;
    server.use(http.get("/api/v1/clients", () => { asked++; return HttpResponse.json({ error: "boom" }, { status: 500 }); }));
    mount();
    expect(await screen.findByText("Your client appears under Clients once it connects.")).toBeInTheDocument();
    await pause(2500);
    expect(asked).toBe(1);
  }, 15000);
});

describe("Connect a client — other ways to connect", () => {
  it("is a disclosure, closed by default", async () => {
    mount();
    const button = otherWays();
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("heading", { name: "burrow.yaml" })).toBeNull();
    expect(screen.queryByRole("link", { name: /amd64/ })).toBeNull();
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(document.getElementById(button.getAttribute("aria-controls")!)).toContainElement(screen.getByRole("heading", { name: "burrow.yaml" }));
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("heading", { name: "burrow.yaml" })).toBeNull();
  });

  it("holds the manual downloads, per system, served by this relay", async () => {
    await mountOther();
    const section = (await screen.findByRole("heading", { name: "Download by hand" })).closest("section")!;
    const links = await within(section).findAllByRole("link");
    expect(links.map((a) => [a.getAttribute("aria-label") ?? a.textContent, a.getAttribute("href")])).toEqual([
      ["Linux amd64", "/download/burrow/linux/amd64"],
      ["Linux arm64", "/download/burrow/linux/arm64"],
      ["Linux arm", "/download/burrow/linux/arm"],
      ["Linux 386", "/download/burrow/linux/386"],
      ["macOS amd64", "/download/burrow/darwin/amd64"],
      ["macOS arm64", "/download/burrow/darwin/arm64"],
      ["Windows amd64", "/download/burrow/windows/amd64"],
      ["Windows 386", "/download/burrow/windows/386"],
      ["checksums.txt", "/download/burrow/checksums.txt"],
    ]);
    // The checksum guards against a damaged download; it proves nothing about the origin.
    expect(section.textContent).toMatch(/complete and undamaged/);
    expect(section.textContent).not.toMatch(/authentic|genuine|verif|trust/i);
  });

  it("a relay built from an untagged commit offers the rolling builds only", async () => {
    db.discovery.version = "develop";
    await mountOther();
    const section = (await screen.findByRole("heading", { name: "Download by hand" })).closest("section")!;
    await within(section).findByRole("link", { name: "Linux amd64" });
    expect(within(section).getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual([
      "/download/burrow/linux/amd64", "/download/burrow/linux/arm64",
      "/download/burrow/darwin/arm64", "/download/burrow/windows/amd64",
      "/download/burrow/checksums.txt",
    ]);
  });

  it("holds a burrow.yaml example with the relay's control endpoint and a token file, not a token", async () => {
    await mountOther();
    const section = screen.getByRole("heading", { name: "burrow.yaml" }).closest("section")!;
    await waitFor(() => expect(section.querySelector("pre code")!.textContent).toContain("server: relay.example.com:7000"));
    const yaml = section.querySelector("pre code")!.textContent!;
    expect(yaml).toContain("token_file:");
    expect(yaml).toContain("services:");
    expect(yaml).not.toMatch(/^token:/m);
    expect(section.textContent).toContain("burrow connect --config burrow.yaml");
  });

  it("holds exactly two parts: the downloads and the burrow.yaml example", async () => {
    await mountOther();
    const panel = document.getElementById(otherWays().getAttribute("aria-controls")!)!;
    expect(within(panel).getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual(["Download by hand", "burrow.yaml"]);
  });

  // The form that asked for a local address, a public port, a protocol and a
  // client name, and minted a token for a `burrow connect` line, is gone: the
  // three lines publish, and a token is made under Tokens.
  it("has no second form and mints no token", async () => {
    let minted = 0;
    server.use(http.post("/api/v1/tokens", () => { minted++; return HttpResponse.json({ name: "x", token: "bur_x" }, { status: 201 }); }));
    const feed = clientsFeed([]);
    await mountOther();
    for (const label of [/local address/i, /public port/i, /protocol/i, /client name/i]) {
      expect(screen.queryByLabelText(label)).toBeNull();
    }
    expect(screen.queryByRole("button", { name: /generate token/i })).toBeNull();
    expect(screen.queryByRole("heading", { name: /what to expose|name this client|credentials|run on the client/i })).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(document.body.textContent).not.toMatch(/bur_|--token bur/);
    expect(minted).toBe(0);
    // Its poller went with it: the clients are asked for by the waiting line alone.
    await waitFor(() => expect(feed.asked).toBeGreaterThanOrEqual(1));
    await pause(2500);
    expect(feed.asked).toBeLessThanOrEqual(3);
  }, 15000);

  it("says where a token for a machine without a browser is made", async () => {
    await mountOther();
    const panel = document.getElementById(otherWays().getAttribute("aria-controls")!)!;
    const line = within(panel).getByText(/Need a token for a machine without a browser\?/);
    expect(line).toHaveTextContent("Need a token for a machine without a browser? Create one under Clients → Tokens.");
    expect(within(line).getByRole("link", { name: "Clients → Tokens" })).toHaveAttribute("href", "/clients?tab=tokens");
  });

  it.each([
    ["too many requests", 429],
    ["a relay error", 500],
  ])("when the relay cannot be asked for its builds (%s) says so and offers to try again", async (_label, status) => {
    let fail = true;
    server.use(http.get("/api/v1/client/discovery", () => fail
      ? HttpResponse.json({ error: "boom" }, { status })
      : HttpResponse.json(db.discovery)));
    await mountOther();
    const section = screen.getByRole("heading", { name: "Download by hand" }).closest("section")!;
    expect(await within(section).findByText("Could not ask the relay which builds it offers.")).toBeInTheDocument();
    expect(within(section).queryByText(/Asking the relay/)).toBeNull();
    // Not an old relay: no notice, and the section was opened by hand only.
    expect(screen.queryByText(/older than the client commands/)).toBeNull();
    fail = false;
    await userEvent.click(within(section).getByRole("button", { name: "Try again" }));
    expect(await within(section).findByRole("link", { name: "Linux amd64" })).toHaveAttribute("href", "/download/burrow/linux/amd64");
    expect(within(section).queryByText(/Could not ask the relay/)).toBeNull();
  });

  it("says the same when the relay cannot be reached at all", async () => {
    server.use(http.get("/api/v1/client/discovery", () => HttpResponse.error()));
    await mountOther();
    expect(await screen.findByText("Could not ask the relay which builds it offers.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("on a relay without the discovery endpoint says so and opens the section", async () => {
    server.use(http.get("/api/v1/client/discovery", () => HttpResponse.json({ error: "not found" }, { status: 404 })));
    mount();
    expect(await screen.findByText("This relay is older than the client commands below; use 'Other ways to connect'.")).toBeInTheDocument();
    await waitFor(() => expect(otherWays()).toHaveAttribute("aria-expanded", "true"));
    expect(screen.getByRole("heading", { name: "burrow.yaml" })).toBeInTheDocument();
    // Such a relay serves no downloads either.
    const section = screen.getByRole("heading", { name: "Download by hand" }).closest("section")!;
    expect(within(section).queryAllByRole("link")).toEqual([]);
    // The section can still be closed by hand.
    await userEvent.click(otherWays());
    expect(otherWays()).toHaveAttribute("aria-expanded", "false");
  });

  it("has no such notice on a current relay, nor while the answer is out", async () => {
    mount();
    await screen.findByText("Waiting for your client…");
    expect(screen.queryByText(/older than the client commands/)).toBeNull();
    expect(otherWays()).toHaveAttribute("aria-expanded", "false");
  });
});

describe("Connect a client — the intro", () => {
  it("renders a 'Manage tokens' link pointing to the Tokens tab of Clients", () => {
    mount();
    expect(screen.getByRole("link", { name: /^manage tokens$/i })).toHaveAttribute("href", "/clients?tab=tokens");
  });

  it("says what a client is", () => {
    mount();
    expect(screen.getByText(/machine running/i)).toBeInTheDocument();
  });

  it("without a service or a provider in the address names none", () => {
    mount();
    expect(screen.queryByText(/from this machine\./)).toBeNull();
    expect(screen.queryByText(/no longer exists/)).toBeNull();
  });
});

// What GET /ai/providers/<slug> says about the provider's state, one answer after the other.
function providerFeed(slug: string, states: string[]) {
  const feed = { asked: 0 };
  server.use(http.get(`/api/v1/ai/providers/${slug}`, () => {
    const state = states[Math.min(feed.asked, states.length - 1)]!;
    feed.asked++;
    const row = db.aiProviders.find((p) => p.slug === slug)!;
    return HttpResponse.json({ ...row, status: state === "online" ? "Connected" : "Offline", state, model_count: 0 });
  }));
  return feed;
}

describe("Connect a client — for a service or a provider", () => {
  it("?service=<id>: the run line serves that service, on the port it was last served from", async () => {
    const user = userEvent.setup();
    mount("/clients/connect?service=svc_ai001");
    const intro = await screen.findByText(/from this machine\./);
    expect(intro).toHaveTextContent("Serve ollama from this machine.");
    expect(within(intro).getByText("ollama").tagName).toBe("STRONG");
    expect(lineTexts()).toEqual([
      `curl -fsSL ${window.location.origin}/install.sh | sh`,
      `burrow login ${window.location.origin}`,
      "burrow http 11434 --name ollama",
    ]);
    await user.click(screen.getByRole("button", { name: "Copy run command" }));
    expect(await navigator.clipboard.readText()).toBe("burrow http 11434 --name ollama");
    // A service has no state of its own to wait on here: the next client counts.
    expect(await screen.findByText("Waiting for your client…")).toBeInTheDocument();
  });

  it("?service=<id>: never shows the command of another service while the answer is out", async () => {
    let release = () => {};
    const held = new Promise<void>((r) => { release = r; });
    server.use(http.get("/api/v1/services/svc_ai001", async () => {
      await held;
      return undefined;
    }));
    mount("/clients/connect?service=svc_ai001");
    await pause(100);
    expect(lineTexts()).toEqual([]);
    expect(document.body.textContent).not.toContain("burrow http 3000");
    release();
    await waitFor(() => expect(lineTexts()[2]).toBe("burrow http 11434 --name ollama"));
  });

  it("?service=<id>: a service that was never served gets the example port", async () => {
    db.services.find((x) => x.id === "svc_graf01")!.local_addr = "";
    mount("/clients/connect?service=svc_graf01");
    await screen.findByText(/from this machine\./);
    expect(lineTexts()[2]).toBe("burrow http 3000 --name grafana");
    // Nothing was reported, so nothing was left out.
    expect(screen.queryByText(/cannot stand in a command/)).toBeNull();
  });

  // The provider's page and the service's page show this port for the same service.
  it("?service=<id>: a model server that was never served gets the port its provider's page shows", async () => {
    mount("/clients/connect?service=svc_lms001");
    await screen.findByText(/from this machine\./);
    expect(lineTexts()[2]).toBe("burrow http 11434 --name lmstudio");
    expect(screen.queryByText(/cannot stand in a command/)).toBeNull();
  });

  // Real addresses the command does not take: the reader is told that the port shown is not theirs.
  it.each(["my_host:8000", "[fe80::1%eth0]:8000"])("?service=<id>: says so when the reported address %j is not used", async (reported) => {
    db.services.find((x) => x.id === "svc_graf01")!.local_addr = reported;
    mount("/clients/connect?service=svc_graf01");
    await screen.findByText(/from this machine\./);
    expect(lineTexts()[2]).toBe("burrow http 3000 --name grafana");
    expect(screen.getByText(/cannot stand in a command/)).toHaveTextContent(
      "The address this service was last served from cannot stand in a command. The run line shows port 3000 instead: put in where the server listens.",
    );
    // What was reported is not repeated on the page.
    expect(document.body.textContent).not.toContain(reported);
  });

  it("?service=<id>: an address on another host is kept whole", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.local_addr = "192.168.1.20:8000";
    mount("/clients/connect?service=svc_ai001");
    await screen.findByText(/from this machine\./);
    expect(lineTexts()[2]).toBe("burrow http 192.168.1.20:8000 --name ollama");
  });

  // The address is what the client reported; the relay does not check it. In
  // PowerShell a POSIX-quoted `x'\'';calc;'\''y:80` would run calc.
  it.each([
    "x';calc;'y:80", "x;calc:80", "$(calc):80", "x`calc`:80", "x|calc:80", "x & calc:80", "127.0.0.1;calc:80", "http://x:80", "x:99999",
  ])("?service=<id>: an address that is no host:port (%j) reaches no command, on any operating system", async (hostile) => {
    db.services.find((x) => x.id === "svc_ai001")!.local_addr = hostile;
    mount("/clients/connect?service=svc_ai001");
    await screen.findByText(/from this machine\./);
    for (const os of ["Linux", "macOS", "Windows"]) {
      await userEvent.click(screen.getByRole("radio", { name: os }));
      expect(screen.getByRole("radio", { name: os })).toHaveAttribute("aria-checked", "true");
      const run = lineTexts()[2]!;
      // ollama backs a provider: the port of a model server stands in for the address.
      expect(run, os).toBe("burrow http 11434 --name ollama");
      // Nothing a shell would act on: no quote, separator, pipe, substitution or blank beyond the plain words.
      expect(run, os).toMatch(/^burrow http [A-Za-z0-9.:-]+ --name [a-z0-9][a-z0-9_-]*$/);
      expect(document.querySelector(".install-lines")!.textContent, os).not.toContain("calc");
    }
    expect(screen.getByText(/cannot stand in a command/)).toHaveTextContent("The run line shows port 11434 instead");
  });

  it("?service=<id>: a name that begins with a hyphen is not put into the command as a flag", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.name = "--config";
    mount("/clients/connect?service=svc_ai001");
    expect(await screen.findByText("The name of that service cannot stand in a command: the lines below publish a new service.")).toBeInTheDocument();
    expect(lineTexts()[2]).toBe("burrow http 3000");
  });

  it("?service=<id>: a name that cannot stand in a command is not claimed to be served", async () => {
    db.services.find((x) => x.id === "svc_ai001")!.name = "My model; rm -rf";
    mount("/clients/connect?service=svc_ai001");
    expect(await screen.findByText("The name of that service cannot stand in a command: the lines below publish a new service.")).toBeInTheDocument();
    expect(lineTexts()[2]).toBe("burrow http 3000");
    expect(screen.queryByText(/from this machine\./)).toBeNull();
    expect(document.querySelector(".install-lines")!.textContent).not.toContain("rm -rf");
  });

  it("?service=<id>: a TCP service is not claimed to be served by these lines", async () => {
    mount("/clients/connect?service=svc_pg001");
    expect(await screen.findByText("That service is not an HTTP service: the lines below do not serve it.")).toHaveClass("muted");
    expect(lineTexts()[2]).toBe("burrow http 3000");
    expect(screen.queryByText(/from this machine\./)).toBeNull();
  });

  it("?service=<id>: an unknown id shows the plain page and says so", async () => {
    mount("/clients/connect?service=gone");
    const note = await screen.findByText("That service no longer exists.");
    expect(note).toHaveClass("muted");
    expect(lineTexts()).toEqual([
      `curl -fsSL ${window.location.origin}/install.sh | sh`,
      `burrow login ${window.location.origin}`,
      "burrow http 3000",
    ]);
    expect(screen.queryByText(/from this machine\./)).toBeNull();
    expect(await screen.findByText("Waiting for your client…")).toBeInTheDocument();
  });

  it("?provider=<slug>: the run line serves the provider's service and the waiting line watches the provider", async () => {
    const clients = clientsFeed([]);
    const feed = providerFeed("llamacpp", ["offline", "offline", "online"]);
    mount("/clients/connect?provider=llamacpp");
    const intro = await screen.findByText(/from this machine\./);
    expect(intro).toHaveTextContent("Serve llamacpp from this machine.");
    // The same line as on the provider's page, which links here.
    expect(lineTexts()[2]).toBe("burrow http 11434 --name llamacpp");
    const waiting = await screen.findByText("Waiting for the client…");
    const status = waiting.closest("[role=status]") as HTMLElement;
    expect(status).not.toBeNull();
    // Not the arrival of any client: this provider.
    expect(screen.queryByText("Waiting for your client…")).toBeNull();
    await waitFor(() => expect(within(status).getByText("Online")).toBeInTheDocument(), { timeout: 8000 });
    expect(feed.asked).toBeGreaterThanOrEqual(3);
    expect(clients.asked).toBe(0);
  }, 20000);

  it("?provider=<slug>: takes the port from the provider's service", async () => {
    providerFeed("ollama", ["online"]);
    mount("/clients/connect?provider=ollama");
    await screen.findByText(/from this machine\./);
    expect(lineTexts()[2]).toBe("burrow http 11434 --name ollama");
    expect(await screen.findByText("Online")).toBeInTheDocument();
  });

  it("?provider=<slug>: an unknown slug shows the plain page and says so", async () => {
    mount("/clients/connect?provider=gone");
    expect(await screen.findByText("That provider no longer exists.")).toHaveClass("muted");
    expect(lineTexts()[2]).toBe("burrow http 3000");
    expect(await screen.findByText("Waiting for your client…")).toBeInTheDocument();
  });

  it("?provider=<slug>: a provider whose service is gone says that the service is", async () => {
    server.use(http.get("/api/v1/services/svc_llama01", () => HttpResponse.json({ error: "service not found" }, { status: 404 })));
    mount("/clients/connect?provider=llamacpp");
    expect(await screen.findByText("That service no longer exists.")).toBeInTheDocument();
    expect(lineTexts()[2]).toBe("burrow http 3000");
  });

  it("?provider=<slug>: a hosted API is served by no machine", async () => {
    mount("/clients/connect?provider=zai");
    expect(await screen.findByText("That provider is a hosted API: no machine serves it.")).toBeInTheDocument();
    expect(lineTexts()[2]).toBe("burrow http 3000");
    expect(screen.queryByText(/from this machine\./)).toBeNull();
  });

  it("a custom role with tunnels:read:any waits for the client, and asks for the list", async () => {
    db.me = { id: "u_op", email: "op@acme.io", role: "operator", permissions: ["tunnels:read:any", "tokens:manage:own"] };
    const feed = clientsFeed([]);
    mount();
    expect(await screen.findByText("Waiting for your client…")).toBeInTheDocument();
    await waitFor(() => expect(feed.asked).toBeGreaterThan(0));
  });
});

describe("Connect a client — a gRPC service", () => {
  it("offers HTTP and gRPC; gRPC shows burrow grpc 50051 and says what it does", async () => {
    const user = userEvent.setup();
    mount();
    const kind = screen.getByRole("radiogroup", { name: "Kind of service" });
    expect(within(kind).getAllByRole("radio").map((r) => r.textContent)).toEqual(["Web app or API", "gRPC"]);
    expect(within(kind).getByRole("radio", { name: "Web app or API" })).toHaveAttribute("aria-checked", "true");
    await user.click(within(kind).getByRole("radio", { name: "gRPC" }));
    expect(lineTexts()[2]).toBe("burrow grpc 50051");
    expect(screen.getByText(/speaks HTTP\/2 to it/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy run command" }));
    expect(await navigator.clipboard.readText()).toBe("burrow grpc 50051");
    await user.click(within(kind).getByRole("radio", { name: "Web app or API" }));
    expect(lineTexts()[2]).toBe("burrow http 3000");
  });

  it("keeps the install and sign-in lines as they are", async () => {
    mount();
    await userEvent.click(screen.getByRole("radio", { name: "gRPC" }));
    expect(lineTexts().slice(0, 2)).toEqual([
      `curl -fsSL ${window.location.origin}/install.sh | sh`,
      `burrow login ${window.location.origin}`,
    ]);
  });
});
