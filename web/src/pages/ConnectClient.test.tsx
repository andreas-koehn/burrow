import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Routes, Route } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import ConnectClient from "@/pages/ConnectClient";
import type { ClientView } from "@/lib/contract";

function mount() {
  return renderApp(
    <Routes><Route path="/clients/connect" element={<ConnectClient />} /></Routes>,
    "/clients/connect",
  );
}

const otherWays = () => screen.getByRole("button", { name: "Other ways to connect" });

// The previous form lives behind "Other ways to connect".
async function mountOther() {
  const r = mount();
  await userEvent.click(otherWays());
  return r;
}

// Helper: gate like handlers.ts does (only csrf-gate non-safe methods; admin always required for /clients).
// GET /clients is admin-gated but NOT csrf-gated (GET is a safe method).
function adminGate(req: Request) {
  const method = req.method.toUpperCase();
  if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS") {
    if (req.headers.get("X-CSRF-Token") !== db.csrf) {
      return HttpResponse.json({ error: "csrf token invalid" }, { status: 403 });
    }
  }
  return null;
}

// Wait until the "Run on the client" section is visible (command shown), then return the pre code element.
async function waitForCommandSection(): Promise<Element> {
  await screen.findByRole("heading", { name: /run on the client/i });
  const pre = document.querySelector("pre#connect-command code");
  if (!pre) throw new Error("pre#connect-command code not found");
  return pre;
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
      `burrow login ${window.location.origin} --token -`,
      "burrow http 3000",
    ]);
    const lines = document.querySelector(".install-lines")!;
    expect(lines.compareDocumentPosition(otherWays()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    for (const name of ["Copy install command", "Copy sign-in command", "Copy run command"]) {
      expect(screen.getByRole("button", { name })).toBeInTheDocument();
    }
  });

  it("leads to the Tokens tab for the token the sign-in asks for", () => {
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
    db.me.role = "user";
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
    expect(screen.queryByLabelText(/client name/i)).toBeNull();
    expect(screen.queryByRole("link", { name: /amd64/ })).toBeNull();
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(document.getElementById(button.getAttribute("aria-controls")!)).toContainElement(screen.getByLabelText(/client name/i));
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByLabelText(/client name/i)).toBeNull();
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

  it("mints no token until one is asked for inside it", async () => {
    let minted = 0;
    server.use(http.post("/api/v1/tokens", () => { minted++; return HttpResponse.json({ name: "edge-01", token: "bur_test_0000" }, { status: 201 }); }));
    mount();
    await screen.findByText("Waiting for your client…");
    await userEvent.click(screen.getByRole("button", { name: "Copy sign-in command" }));
    await userEvent.click(otherWays());
    expect(minted).toBe(0);
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(minted).toBe(1);
  });

  it("the connect command never holds the token, masked or not; the token has its own field", async () => {
    server.use(http.post("/api/v1/tokens", () => HttpResponse.json({ name: "edge-01", token: "bur_test_0000" }, { status: 201 })));
    const user = userEvent.setup();
    mount();
    await user.click(otherWays());
    await user.type(screen.getByLabelText(/client name/i), "edge-01");
    await user.click(screen.getByRole("button", { name: /generate token/i }));
    const cmd = await waitForCommandSection();
    expect(cmd.textContent).toContain('--token "$BURROW_TOKEN"');
    await user.click(screen.getByRole("button", { name: "Reveal token" }));
    expect(screen.getByText("bur_test_0000")).toBeInTheDocument();
    expect(cmd.textContent).not.toContain("bur_");
    await user.click(screen.getByRole("button", { name: "Copy connect command" }));
    const copied = await navigator.clipboard.readText();
    expect(copied).toBe(cmd.textContent);
    expect(copied).not.toContain("bur_");
    await user.click(screen.getByRole("button", { name: "Copy client token" }));
    expect(await navigator.clipboard.readText()).toBe("bur_test_0000");
  });

  it("on a relay without the discovery endpoint says so and opens the section", async () => {
    server.use(http.get("/api/v1/client/discovery", () => HttpResponse.json({ error: "not found" }, { status: 404 })));
    mount();
    expect(await screen.findByText("This relay is older than the client commands below; use 'Other ways to connect'.")).toBeInTheDocument();
    await waitFor(() => expect(otherWays()).toHaveAttribute("aria-expanded", "true"));
    expect(screen.getByLabelText(/client name/i)).toBeInTheDocument();
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

describe("Connect a client — existing contracts", () => {
  it("mints a token for the named client and reveals it once", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    // The token display inside .mono code (not the command pre)
    const tokenEl = await screen.findByText(/^bur_••••••••$/);
    expect(tokenEl).toBeInTheDocument();
    expect(screen.getByText(/store this token now/i)).toBeInTheDocument();
  });

  it("shows the install command containing the client name", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const cmdEl = await waitForCommandSection();
    expect(cmdEl.textContent).toMatch(/--name edge-01/);
  });

  it("shows the relay server endpoint from connect-info in the install command", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    // The endpoint appears in the Credentials section as a <code class="mono">
    expect(screen.getByText("relay.example.com:7000")).toBeInTheDocument();
  });
});

describe("P2.2 — Step fields (local/remote/protocol)", () => {
  it("renders local address and public port fields", async () => {
    await mountOther();
    expect(screen.getByLabelText(/local address/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/public port/i)).toBeInTheDocument();
  });

  it("renders protocol Select with TCP default", async () => {
    await mountOther();
    // DS Select renders the trigger button; visible text is the selected label
    expect(screen.getByText("TCP")).toBeInTheDocument();
  });

  it("disables the remote field when HTTP is selected", async () => {
    await mountOther();
    // Find the protocol Select trigger (button showing current selection "TCP")
    const protocolTrigger = screen.getByText("TCP").closest("button")!;
    await userEvent.click(protocolTrigger);
    const httpOption = await screen.findByRole("option", { name: "HTTP" });
    await userEvent.click(httpOption);
    expect(screen.getByLabelText(/public port/i)).toBeDisabled();
  });
});

describe("P2.3 — Command built from real fields", () => {
  it("tcp: custom local + remote appear in command", async () => {
    await mountOther();
    // Set local address to custom value
    const localInput = screen.getByLabelText(/local address/i);
    await userEvent.clear(localInput);
    await userEvent.type(localInput, "127.0.0.1:8080");
    // Set remote port
    const remoteInput = screen.getByLabelText(/public port/i);
    await userEvent.clear(remoteInput);
    await userEvent.type(remoteInput, "5000");
    // Mint
    await userEvent.type(screen.getByLabelText(/client name/i), "test-tcp");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const cmdEl = await waitForCommandSection();
    expect(cmdEl.textContent).toMatch(/--local 127\.0\.0\.1:8080/);
    expect(cmdEl.textContent).toMatch(/--remote 5000/);
    expect(cmdEl.textContent).not.toMatch(/--type http/);
  });

  it("http: switches to --type http and removes --remote", async () => {
    await mountOther();
    // Switch protocol to HTTP
    const protocolTrigger = screen.getByText("TCP").closest("button")!;
    await userEvent.click(protocolTrigger);
    const httpOption = await screen.findByRole("option", { name: "HTTP" });
    await userEvent.click(httpOption);
    // Mint
    await userEvent.type(screen.getByLabelText(/client name/i), "test-http");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const cmdEl = await waitForCommandSection();
    expect(cmdEl.textContent).toMatch(/--type http/);
    expect(cmdEl.textContent).not.toMatch(/--remote/);
  });
});

describe("P2.4 — Wrapped command render + Copy", () => {
  it("pre element has cmd-block and wrap classes", async () => {
    const { container } = await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-02");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(container.querySelector("pre#connect-command.cmd-block.wrap")).not.toBeNull();
  });

  it("copy connect command button is present", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-02");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(screen.getByRole("button", { name: /copy connect command/i })).toBeInTheDocument();
  });
});

describe("P2.5 — Success-loop poller", () => {
  it("shows 'Waiting for … to connect' after mint (admin)", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-99");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    // The success-loop status div is the one inside the "Run on the client" section.
    // The page may have multiple role=status elements (notice-inline + our div).
    // Query by text content of the waiting state.
    const waitingText = await screen.findByText(/waiting for\s+to connect/i);
    expect(waitingText).toBeInTheDocument();
    // Ensure the client name appears inside that same status region
    expect(waitingText.closest("[role=status]")).toBeTruthy();
    expect(waitingText.closest("[role=status]")!.textContent).toContain("edge-99");
  });

  it("shows 'View client' link once a matching client appears", async () => {
    // Override /clients handler to return a matching client
    server.use(
      http.get("/api/v1/clients", ({ request }) => {
        const g = adminGate(request);
        if (g) return g;
        const client: ClientView = {
          session_id: "sess_new",
          token_name: "edge-99",
          user_id: "u",
          remote_addr: "1.2.3.4:5000",
          os: "linux",
          arch: "amd64",
          client_version: "0.5.0",
          service_count: 0,
          total_bytes_in: 0,
          total_bytes_out: 0,
        };
        return HttpResponse.json([client]);
      }),
    );

    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-99");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();

    // Wait for the "View client" link to appear (poller fires and finds matching client).
    // Allow up to 5s to cover the initial query fetch after the mutation resolves.
    const link = await screen.findByRole("link", { name: /view client/i }, { timeout: 5000 });
    expect(link).toHaveAttribute("href", expect.stringContaining("/clients/sess_new"));
  });
});

describe("P4.4 — Manage tokens link", () => {
  it("renders a 'Manage tokens' link pointing to the Tokens tab of Clients", async () => {
    await mountOther();
    const link = screen.getByRole("link", { name: /^manage tokens$/i });
    expect(link).toHaveAttribute("href", "/clients?tab=tokens");
  });
});

describe("P2.6 — Inline explainers", () => {
  it("shows the 'machine running burrow connect' explainer", async () => {
    await mountOther();
    expect(screen.getByText(/machine running/i)).toBeInTheDocument();
  });

  it("shows the 'reachable address' explainer after minting", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-03");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(screen.getByText(/reachable address/i)).toBeInTheDocument();
  });
});

describe("Connect a client — validation and flow (F5/F6)", () => {
  // jsdom has no scrollIntoView; one test installs a mock, so put the original back.
  const originalScrollIntoView = Element.prototype.scrollIntoView;
  afterEach(() => {
    Element.prototype.scrollIntoView = originalScrollIntoView;
    vi.restoreAllMocks();
  });

  it("rejects an invalid name and does not mint a token", async () => {
    let minted = 0;
    server.use(http.post("/api/v1/tokens", () => { minted++; return HttpResponse.json({ name: "x", token: "bur_x" }, { status: 201 }); }));
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "UI Audit Test!");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    expect(await screen.findByText("Use lowercase letters, digits and hyphens only.")).toBeInTheDocument();
    expect(minted).toBe(0);
    expect(screen.queryByRole("heading", { name: /credentials/i })).toBeNull();
  });

  it("locks the name and replaces Generate with 'Connect another client' after minting", async () => {
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(screen.getByLabelText(/client name/i)).toBeDisabled();
    expect(screen.queryByRole("button", { name: /generate token/i })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /connect another client/i }));
    expect(screen.getByLabelText(/client name/i)).not.toBeDisabled();
    expect(screen.getByLabelText(/client name/i)).toHaveValue("");
    expect(screen.getByLabelText(/client name/i)).toHaveFocus();
    expect(screen.queryByRole("heading", { name: /credentials/i })).toBeNull();
  });

  it.each([
    ["an empty name", "", "Enter a name."],
    ["a whitespace-only name", "   ", "Use lowercase letters, digits and hyphens only."],
  ])("surfaces the validation message for %s and does not mint", async (_label, value, msg) => {
    let minted = 0;
    server.use(http.post("/api/v1/tokens", () => { minted++; return HttpResponse.json({ name: "x", token: "bur_x" }, { status: 201 }); }));
    await mountOther();
    if (value) await userEvent.type(screen.getByLabelText(/client name/i), value);
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    expect(await screen.findByText(msg)).toBeInTheDocument();
    expect(minted).toBe(0);
    expect(screen.queryByRole("heading", { name: /credentials/i })).toBeNull();
  });

  it("moves focus to the credentials heading and scrolls it into view", async () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    const focus = vi.spyOn(HTMLElement.prototype, "focus");
    await mountOther();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const heading = await screen.findByRole("heading", { name: /credentials/i });
    expect(heading).toHaveFocus();
    expect(scrollIntoView).toHaveBeenCalled();
    // focus() must not cancel the smooth scroll that precedes it.
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  });

  it("shows 'What to expose' before 'Name this client'", async () => {
    await mountOther();
    const headings = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);
    expect(headings.indexOf("1. What to expose")).toBeLessThan(headings.indexOf("2. Name this client"));
  });

  it("shell-quotes a local address that needs it", async () => {
    await mountOther();
    const local = screen.getByLabelText(/local address/i);
    await userEvent.clear(local);
    await userEvent.type(local, "my host:3000");
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const cmd = await waitForCommandSection();
    expect(cmd.textContent).toContain("--local 'my host:3000'");
  });
});
