import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { ALL_PERMISSIONS } from "@/auth/permissions";
import { useLocation } from "react-router-dom";
import AutomationTokens from "@/pages/AutomationTokens";

function Where() {
  const l = useLocation();
  return <span data-testid="where">{l.pathname + l.search}</span>;
}

function mount() {
  return renderApp(<AutomationTokens />, "/settings/automation");
}

describe("Automation tokens", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders a real EmptyState (not a flat tr) when there are no tokens (C3)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.includes("/automation/tokens")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      return new Response(JSON.stringify([]), { status: 200 }) as Response;
    });
    const { container } = mount();
    await waitFor(() => {
      expect(container.querySelector(".state-card")).not.toBeNull();
      expect(container.querySelector(".state-card .icon-bubble")).not.toBeNull();
    });
    const card = container.querySelector(".state-card")!;
    expect(card.textContent).not.toContain("Long-lived bearer tokens for CI / CLI / bots");
    expect(card.textContent).toMatch(/mint a token/i);
  });

  it("says when the tokens cannot be loaded instead of loading for ever", async () => {
    server.use(http.get("/api/v1/automation/tokens", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/Couldn't load the automation tokens/);
  });

  it("renders a row per token with mono bua_ prefix", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /automation tokens/i });
    expect(within(table).getByText(/bua_/)).toBeInTheDocument();
  });

  it("writes when a token was last used as every table does, and never for one that does not expire", async () => {
    mount();
    const row = within(await screen.findByRole("table", { name: /automation tokens/i })).getByText("ci-runner").closest("tr")!;
    const used = row.querySelector("time")!;
    // Long ago: the date, with the exact time on hover; not the relay's raw timestamp.
    expect(used).toHaveTextContent("18 May 2026");
    expect(used).toHaveAttribute("datetime", "2026-05-18T09:00:00Z");
    expect(used.getAttribute("title")).toMatch(/09:00:00/);
    expect(row).not.toHaveTextContent("2026-05-18T09:00:00Z");
    expect(within(row).getByText("never")).toBeInTheDocument();
  });

  // P4.3 — reciprocal hint
  it("says these tokens are for scripts, not clients (P4.3)", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }) as Response);
    mount();
    const hint = screen.getByText(/These authenticate scripts, not clients\./);
    expect(hint).toHaveTextContent(
      "These authenticate scripts, not clients. To connect a machine that runs burrow, use Client tokens. Apps that call a model use a gateway key.",
    );
  });

  it("points apps that call a model to gateway keys", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }) as Response);
    mount();
    expect(screen.getByRole("link", { name: "gateway key" })).toHaveAttribute("href", "/gateway/keys");
  });

  it("renders Client tokens cross-link pointing to the Tokens tab of Clients (P4.3)", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }) as Response);
    mount();
    const link = screen.getByRole("link", { name: /client tokens/i });
    expect(link).toHaveAttribute("href", "/clients?tab=tokens");
  });

  it("Creating a token reveals the plaintext once with the verbatim save-now warning", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new token/i }));
    const dlg = await screen.findByRole("dialog");
    await userEvent.type(within(dlg).getByLabelText(/^name$/i), "ci-bot");
    await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/automation/tokens")
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
    expect(
      await screen.findByText("Save this token now — you won't see it again."),
    ).toBeInTheDocument();
  });

  describe("token dialog presets", () => {
    const readOnly = ALL_PERMISSIONS.filter((p) => p.includes(":read"));
    let posted: Record<string, unknown> | null;

    function captureMint() {
      posted = null;
      server.use(
        http.post("/api/v1/automation/tokens", async ({ request }) => {
          posted = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json(
            {
              token: { id: "at_new", name: "x", prefix: "bua_new", user_id: "u", role_at_mint: "admin", permissions: [], expires_at: null, last_used: null, created_at: "2026-10-01T00:00:00Z" },
              plaintext: "bua_new_secret",
            },
            { status: 201 },
          );
        }),
      );
    }

    async function openDialog() {
      mount();
      await userEvent.click(await screen.findByRole("button", { name: /new token/i }));
      const dlg = await screen.findByRole("dialog");
      await within(dlg).findByRole("radio", { name: "Read-only" });
      return dlg;
    }

    it("opens on Read-only with its description, the count and a hint for the empty name", async () => {
      const dlg = await openDialog();
      expect(within(dlg).getByRole("radio", { name: "Read-only" })).toBeChecked();
      expect(within(dlg).getByText("Reads everything that has a read permission (not services or budgets), changes nothing.")).toBeInTheDocument();
      await waitFor(() =>
        expect(within(dlg).getByRole("button", { name: `Permissions (${readOnly.length})` })).toBeInTheDocument(),
      );
      expect(within(dlg).getByRole("button", { name: /^create$/i })).toBeDisabled();
      expect(within(dlg).getByText("Enter a name.")).toBeInTheDocument();
    });

    it("posts the read-only list once a name is typed", async () => {
      captureMint();
      const dlg = await openDialog();
      await userEvent.type(within(dlg).getByLabelText(/^name$/i), "ci-bot");
      await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
      await waitFor(() => expect(posted).not.toBeNull());
      expect(posted!.permissions).toEqual(readOnly);
      expect(posted!.name).toBe("ci-bot");
    });

    it("moves between Operator and Custom as boxes change", async () => {
      const dlg = await openDialog();
      await userEvent.click(within(dlg).getByRole("radio", { name: "Operator" }));
      const operator = ALL_PERMISSIONS.length - 6;
      expect(await within(dlg).findByRole("button", { name: `Permissions (${operator})` })).toBeInTheDocument();
      await userEvent.click(within(dlg).getByRole("button", { name: /^Permissions/ }));
      await userEvent.click(within(dlg).getByRole("checkbox", { name: "audit:read" }));
      expect(within(dlg).getByRole("radio", { name: "Custom" })).toBeChecked();
      expect(within(dlg).getByRole("button", { name: `Permissions (${operator - 1})` })).toBeInTheDocument();
      await userEvent.click(within(dlg).getByRole("checkbox", { name: "audit:read" }));
      expect(within(dlg).getByRole("radio", { name: "Operator" })).toBeChecked();
    });

    it("says what an empty list can do", async () => {
      const dlg = await openDialog();
      await userEvent.click(within(dlg).getByRole("button", { name: /^Permissions/ }));
      for (const p of readOnly) await userEvent.click(within(dlg).getByRole("checkbox", { name: p }));
      expect(within(dlg).getByText("This token can only read its own profile.")).toBeInTheDocument();
    // Twelve userEvent clicks, each slow under a loaded machine.
    }, 30000);

    it("disables a permission the reader does not hold", async () => {
      db.me.permissions = ALL_PERMISSIONS.filter((p) => p !== "users:manage");
      const dlg = await openDialog();
      await userEvent.click(within(dlg).getByRole("button", { name: /^Permissions/ }));
      expect(within(dlg).getByRole("checkbox", { name: "users:manage" })).toBeDisabled();
    });

    it("falls back to inheriting when the relay has no presets route", async () => {
      server.use(http.get("/api/v1/automation/presets", () => HttpResponse.json({ error: "not found" }, { status: 404 })));
      captureMint();
      mount();
      await userEvent.click(await screen.findByRole("button", { name: /new token/i }));
      const dlg = await screen.findByRole("dialog");
      expect(await within(dlg).findByText("The token will inherit your current permissions; the server enforces this too.")).toBeInTheDocument();
      expect(within(dlg).queryByRole("radiogroup")).toBeNull();
      expect(within(dlg).queryByRole("button", { name: /^Permissions/ })).toBeNull();
      await userEvent.type(within(dlg).getByLabelText(/^name$/i), "old");
      await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
      await waitFor(() => expect(posted).not.toBeNull());
      expect(posted).not.toHaveProperty("permissions");
    });

    it("does not mint when the presets fail for a reason other than 404", async () => {
      server.use(http.get("/api/v1/automation/presets", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
      captureMint();
      mount();
      await userEvent.click(await screen.findByRole("button", { name: /new token/i }));
      const dlg = await screen.findByRole("dialog");
      await userEvent.type(within(dlg).getByLabelText(/^name$/i), "x");
      expect(await within(dlg).findByText("The permission presets could not be loaded. Close the dialog and try again.")).toBeInTheDocument();
      expect(within(dlg).getByRole("button", { name: /^create$/i })).toBeDisabled();
      expect(within(dlg).queryByText(/inherit your current permissions/)).toBeNull();
      expect(posted).toBeNull();
    });

    it("names the preset a token matches, else Custom with the count", async () => {
      db.automationTokens.push(
        { id: "at_ro", name: "reader", prefix: "bua_ro", user_id: db.me.id, role_at_mint: "admin", permissions: [...readOnly], expires_at: null, last_used: null, created_at: "2026-04-01T00:00:00Z" },
        { id: "at_cu", name: "mixed", prefix: "bua_cu", user_id: db.me.id, role_at_mint: "admin", permissions: ["audit:read", "metrics:read"], expires_at: null, last_used: null, created_at: "2026-04-01T00:00:00Z" },
      );
      mount();
      const table = await screen.findByRole("table", { name: /automation tokens/i });
      const row = (n: string) => within(table).getByText(n).closest("tr")!;
      await waitFor(() => expect(row("reader")).toHaveTextContent("Read-only"));
      expect(row("mixed")).toHaveTextContent("Custom · 2");
    });

    it("links the saved-token dialog to the MCP page", async () => {
      captureMint();
      const dlg = await openDialog();
      await userEvent.type(within(dlg).getByLabelText(/^name$/i), "ci-bot");
      await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
      expect(await screen.findByRole("link", { name: "Use it in an MCP client" })).toHaveAttribute("href", "/settings/mcp");
    });

    it("leaves the MCP link out for someone who cannot open the MCP page", async () => {
      db.me = { ...db.me, role: "scripter", permissions: ALL_PERMISSIONS.filter((p) => p !== "mcp:tools:read") };
      captureMint();
      const dlg = await openDialog();
      await userEvent.type(within(dlg).getByLabelText(/^name$/i), "ci-bot");
      await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
      expect(await screen.findByText(/Save this token now/)).toBeInTheDocument();
      expect(screen.queryByRole("link", { name: "Use it in an MCP client" })).toBeNull();
    });
  });
});

describe("Automation tokens opened from Settings → MCP", () => {
  it("opens the dialog with the preset in the URL and drops both parameters", async () => {
    const { container } = renderApp(<><AutomationTokens /><Where /></>, "/settings/automation?new=1&preset=operator");
    const dlg = await screen.findByRole("dialog", { name: "New automation token" });
    await waitFor(() => expect(within(dlg).getByRole("radio", { name: "Operator" })).toBeChecked());
    await waitFor(() => expect(container.querySelector("[data-testid=where]")!.textContent).toBe("/settings/automation"));
  });
});
