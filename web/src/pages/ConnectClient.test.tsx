import { describe, it, expect } from "vitest";
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

// Wait until the Install & Run section is visible (command shown), then return the pre code element.
// We detect this by waiting for the "Install & run" heading.
async function waitForCommandSection(): Promise<Element> {
  await screen.findByRole("heading", { name: /install.*run/i });
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
    // The success-loop status div is the one inside the Install & Run section.
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

describe("P4.4 — Back-link to Tokens", () => {
  it("renders a Tokens back-link pointing to /tokens", () => {
    mount();
    const link = screen.getByRole("link", { name: /^tokens$/i });
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
