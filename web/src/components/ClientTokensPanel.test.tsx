import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { ClientTokensPanel } from "./ClientTokensPanel";

function setup() {
  return renderApp(<ClientTokensPanel />, "/clients?tab=tokens");
}

describe("ClientTokensPanel", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("shows the plaintext token once after create", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: any, opts: any) => {
      if (String(url).endsWith("/tokens") && opts?.method === "POST")
        return new Response(JSON.stringify({ name: "laptop", token: "bur_SECRET123" }), { status: 201 }) as any;
      return new Response("[]", { status: 200 }) as any;
    });
    setup();
    // Input is associated with a visible label — query by label text
    await userEvent.type(screen.getByLabelText(/token name/i), "laptop");
    await userEvent.click(screen.getByRole("button", { name: /create/i }));
    expect(await screen.findByText("bur_SECRET123")).toBeInTheDocument();
  });

  it("reveal dialog has a Copy button (L-5)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: any, opts: any) => {
      if (String(url).endsWith("/tokens") && opts?.method === "POST")
        return new Response(JSON.stringify({ name: "laptop", token: "bur_COPY123" }), { status: 201 }) as any;
      return new Response("[]", { status: 200 }) as any;
    });
    setup();
    await userEvent.type(screen.getByLabelText(/token name/i), "laptop");
    await userEvent.click(screen.getByRole("button", { name: /create/i }));
    await screen.findByText("bur_COPY123");
    expect(screen.getByRole("button", { name: /copy token/i })).toBeInTheDocument();
  });

  it("renders a full-width empty-state row when there are no tokens (L-misc)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("[]", { status: 200 }) as any
    );
    setup();
    const cell = (await screen.findByText(/no tokens yet/i)).closest("td")!;
    expect(cell.getAttribute("colspan")).toBe("5");
  });

  it("renders formatted timestamps (not raw RFC3339) for token rows", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "t1", name: "laptop", created_at: "2024-01-15T10:00:00Z", last_used: "2024-06-01T09:30:00Z" },
      ]), { status: 200 }) as any
    );
    setup();
    // Raw RFC3339 strings must NOT appear as-is in the DOM
    await screen.findByText("laptop");
    expect(screen.queryByText("2024-01-15T10:00:00Z")).not.toBeInTheDocument();
    expect(screen.queryByText("2024-06-01T09:30:00Z")).not.toBeInTheDocument();
  });

  it("renders 'never' when last_used is null", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "t1", name: "server", created_at: "2024-01-15T10:00:00Z", last_used: null },
      ]), { status: 200 }) as any
    );
    setup();
    await screen.findByText("server");
    expect(screen.getByText("never")).toBeInTheDocument();
  });

  it("renders the token table using the design-system table.data class", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "t1", name: "laptop", created_at: "2024-01-15T10:00:00Z", last_used: null },
      ]), { status: 200 }) as any
    );
    setup();
    await screen.findByText("laptop");
    expect(screen.getByRole("table").className).toContain("data");
  });

  it("Revoke button has a distinguishing aria-label", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "t1", name: "mymachine", created_at: "2024-01-15T10:00:00Z", last_used: null },
      ]), { status: 200 }) as any
    );
    setup();
    await screen.findByText("mymachine");
    expect(screen.getByRole("button", { name: "Revoke token mymachine" })).toBeInTheDocument();
  });

  it("Revoke variant is destructive (red) (C2)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: any) => {
      if (String(url).includes("/tokens")) {
        return new Response(JSON.stringify([
          { id: "tok1", name: "ci", created_at: "2026-05-25T07:00:00Z", last_used: null },
        ]), { status: 200 }) as any;
      }
      return new Response("[]", { status: 200 }) as any;
    });
    setup();
    await screen.findByText("ci");
    const btn = screen.getByRole("button", { name: /revoke token ci/i });
    expect(btn.className).toMatch(/destructive/);
  });

  it("no token row carries a persistent is-hover highlight (L-misc)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "t1", name: "laptop", created_at: "2024-01-15T10:00:00Z", last_used: null },
        { id: "t2", name: "server", created_at: "2024-03-01T08:00:00Z", last_used: "2024-06-01T09:30:00Z" },
      ]), { status: 200 }) as any
    );
    setup();
    const table = await screen.findByRole("table", { name: /tokens/i });
    table.querySelectorAll("tbody tr").forEach((r) => expect(r.classList.contains("is-hover")).toBe(false));
  });

  it("Revoke opens a confirm dialog; Cancel does NOT call DELETE (C2)", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (url: any, opts?: RequestInit) => {
      const u = String(url);
      if (u.includes("/tokens/tok1") && opts?.method === "DELETE") {
        throw new Error("DELETE should NOT be called when user cancels");
      }
      if (u.includes("/tokens")) {
        return new Response(JSON.stringify([
          { id: "tok1", name: "ci", created_at: "2026-05-25T07:00:00Z", last_used: null },
        ]), { status: 200 }) as any;
      }
      return new Response("[]", { status: 200 }) as any;
    });
    setup();
    await screen.findByText("ci");
    await userEvent.click(screen.getByRole("button", { name: /revoke token ci/i }));
    // Dialog opens
    expect(await screen.findByRole("dialog", { name: /revoke token/i })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^cancel$/i }));
    await waitFor(() => {
      expect(fetchSpy).not.toHaveBeenCalledWith(
        expect.stringContaining("/tokens/tok1"),
        expect.objectContaining({ method: "DELETE" }),
      );
    });
  });

  it("Revoke, once confirmed, deletes the token", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (url: any) => {
      if (String(url).includes("/tokens")) {
        return new Response(JSON.stringify([
          { id: "tok1", name: "ci", created_at: "2026-05-25T07:00:00Z", last_used: null },
        ]), { status: 200 }) as any;
      }
      return new Response("[]", { status: 200 }) as any;
    });
    setup();
    await screen.findByText("ci");
    await userEvent.click(screen.getByRole("button", { name: /revoke token ci/i }));
    const dialog = await screen.findByRole("dialog", { name: /revoke token/i });
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        expect.stringContaining("/tokens/tok1"),
        expect.objectContaining({ method: "DELETE" }),
      );
    });
  });

  // P4.1 — what the list holds. The page header and its Connect-a-client action belong to Clients.
  it("has no page header of its own", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("[]", { status: 200 }) as any);
    setup();
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    expect(screen.queryByRole("link", { name: /connect a client/i })).toBeNull();
  });

  it("explains the token list (P4.1)", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("[]", { status: 200 }) as any);
    setup();
    expect(screen.getByText(/this list includes tokens minted here and via connect a client/i)).toBeInTheDocument();
  });

  it("says what a client token is for and names the three other kinds of credential, each with a link", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("[]", { status: 200 }) as any);
    setup();
    const line = screen.getByText(/A client token lets a machine/);
    expect(line).toHaveTextContent(
      "A client token lets a machine running burrow connect to this relay. Apps that call a model use a gateway key; apps that call one service use that service's API key; scripts use an automation token.",
    );
    // The command is set as code.
    expect(within(line).getByText("burrow").tagName).toBe("CODE");
    expect(within(line).getByRole("link", { name: "gateway key" })).toHaveAttribute("href", "/gateway/keys");
    expect(within(line).getByRole("link", { name: "service" })).toHaveAttribute("href", "/services");
    expect(within(line).getByRole("link", { name: "automation token" })).toHaveAttribute("href", "/settings/automation");
  });

  describe("expiry", () => {
    const listing = (rows: unknown[]) => vi.spyOn(globalThis, "fetch").mockImplementation(async (url: any, opts: any) => {
      if (String(url).endsWith("/tokens") && opts?.method === "POST")
        return new Response(JSON.stringify({ name: "laptop", token: "bur_X", expires_at: null }), { status: 201 }) as any;
      return new Response(JSON.stringify(rows), { status: 200 }) as any;
    });
    const posts = (spy: ReturnType<typeof listing>) =>
      spy.mock.calls.filter(([, o]) => (o as RequestInit | undefined)?.method === "POST").map(([, o]) => JSON.parse(String((o as RequestInit).body)));

    it("shows the date of a token that expires and Never for one that does not", async () => {
      listing([
        { id: "t1", name: "temp", created_at: "2024-01-15T10:00:00Z", last_used: null, expires_at: "2099-03-04T10:00:00Z" },
        { id: "t2", name: "forever", created_at: "2024-01-15T10:00:00Z", last_used: null, expires_at: null },
      ]);
      setup();
      const temp = (await screen.findByText("temp")).closest("tr")!;
      expect(within(temp).getByText(/^Expires 04 Mar 2099/)).toBeInTheDocument();
      const forever = screen.getByText("forever").closest("tr")!;
      expect(within(forever).getByText("Never")).toBeInTheDocument();
    });

    it("an expired token looks expired", async () => {
      listing([{ id: "t1", name: "old", created_at: "2020-01-15T10:00:00Z", last_used: null, expires_at: "2021-03-04T10:00:00Z" }]);
      setup();
      const row = (await screen.findByText("old")).closest("tr")!;
      expect(within(row).getByText(/^Expired 04 Mar 2021/)).toBeInTheDocument();
      expect(row.className).toMatch(/expired/);
    });

    it("sends no expires_at by default", async () => {
      const spy = listing([]);
      setup();
      await userEvent.type(await screen.findByLabelText(/token name/i), "laptop");
      await userEvent.click(screen.getByRole("button", { name: /create/i }));
      await screen.findByText("bur_X");
      expect(posts(spy)).toEqual([{ name: "laptop" }]);
    });

    it.each([["30 days", 30], ["90 days", 90], ["365 days", 365]])("%s sends an expires_at about that far ahead", async (label, days) => {
      const spy = listing([]);
      setup();
      await userEvent.type(await screen.findByLabelText(/token name/i), "laptop");
      await userEvent.click(screen.getByRole("combobox", { name: "Expires" }));
      await userEvent.click(await screen.findByRole("option", { name: label }));
      await userEvent.click(screen.getByRole("button", { name: /create/i }));
      await screen.findByText("bur_X");
      const body = posts(spy)[0];
      const ahead = (Date.parse(body.expires_at) - Date.now()) / 86_400_000;
      expect(ahead).toBeGreaterThan(days - 0.1);
      expect(ahead).toBeLessThan(days + 0.1);
      // The relay refuses more than 365 days ahead: stay inside it.
      expect(Date.parse(body.expires_at)).toBeLessThanOrEqual(Date.now() + 365 * 86_400_000);
    });
  });
});
