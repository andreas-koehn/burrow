import { describe, it, expect, vi, afterEach } from "vitest";
import { screen } from "@testing-library/react";
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
// We detect this by waiting for its heading.
async function waitForCommandSection(): Promise<Element> {
  await screen.findByRole("heading", { name: /run on the client/i });
  const pre = document.querySelector("pre.cmd-block code");
  if (!pre) throw new Error("pre.cmd-block code not found");
  return pre;
}

describe("Connect a client — existing contracts", () => {
  it("mints a token for the named client and reveals it once", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    // The token display inside .mono code (not the command pre)
    const tokenEl = await screen.findByText(/^bur_••••••••$/);
    expect(tokenEl).toBeInTheDocument();
    expect(screen.getByText(/store this token now/i)).toBeInTheDocument();
  });

  it("shows the install command containing the client name", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const cmdEl = await waitForCommandSection();
    expect(cmdEl.textContent).toMatch(/--name edge-01/);
  });

  it("shows the relay server endpoint from connect-info in the install command", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    // The endpoint appears in the Credentials section as a <code class="mono">
    expect(screen.getByText("relay.example.com:7000")).toBeInTheDocument();
  });
});

describe("P2.2 — Step fields (local/remote/protocol)", () => {
  it("renders local address and public port fields", () => {
    mount();
    expect(screen.getByLabelText(/local address/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/public port/i)).toBeInTheDocument();
  });

  it("renders protocol Select with TCP default", () => {
    mount();
    // DS Select renders the trigger button; visible text is the selected label
    expect(screen.getByText("TCP")).toBeInTheDocument();
  });

  it("disables the remote field when HTTP is selected", async () => {
    mount();
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
    mount();
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
    mount();
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
    const { container } = mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-02");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(container.querySelector("pre.cmd-block.wrap")).not.toBeNull();
  });

  it("copy install command button is present", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-02");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    expect(screen.getByRole("button", { name: /copy install command/i })).toBeInTheDocument();
  });
});

describe("P2.5 — Success-loop poller", () => {
  it("shows 'Waiting for … to connect' after mint (admin)", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-99");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await waitForCommandSection();
    // The success-loop status div is the one inside the "Run on the client" section.
    // The page may have multiple role=status elements (notice-inline + our div).
    // Query by text content of the waiting state.
    const waitingText = await screen.findByText(/waiting for/i);
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

    mount();
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
  it("renders a 'Manage tokens' link pointing to /tokens", () => {
    mount();
    const link = screen.getByRole("link", { name: /^manage tokens$/i });
    expect(link).toHaveAttribute("href", "/tokens");
  });
});

describe("P2.6 — Inline explainers", () => {
  it("shows the 'machine running burrow connect' explainer", () => {
    mount();
    expect(screen.getByText(/machine running/i)).toBeInTheDocument();
  });

  it("shows the 'reachable address' explainer after minting", async () => {
    mount();
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
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "UI Audit Test!");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    expect(await screen.findByText("Use lowercase letters, digits and hyphens only.")).toBeInTheDocument();
    expect(minted).toBe(0);
    expect(screen.queryByRole("heading", { name: /credentials/i })).toBeNull();
  });

  it("locks the name and replaces Generate with 'Connect another client' after minting", async () => {
    mount();
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
    mount();
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
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const heading = await screen.findByRole("heading", { name: /credentials/i });
    expect(heading).toHaveFocus();
    expect(scrollIntoView).toHaveBeenCalled();
    // focus() must not cancel the smooth scroll that precedes it.
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  });

  it("shows 'What to expose' before 'Name this client'", () => {
    mount();
    const headings = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(headings.indexOf("1. What to expose")).toBeLessThan(headings.indexOf("2. Name this client"));
  });

  it("shell-quotes a local address that needs it", async () => {
    mount();
    const local = screen.getByLabelText(/local address/i);
    await userEvent.clear(local);
    await userEvent.type(local, "my host:3000");
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    const cmd = await waitForCommandSection();
    expect(cmd.textContent).toContain("--local 'my host:3000'");
  });
});
