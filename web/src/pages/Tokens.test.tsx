import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClientProvider, QueryClient } from "@tanstack/react-query";
import Tokens from "./Tokens";

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><Tokens /></QueryClientProvider>);
}

describe("Tokens", () => {
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
    expect(cell.getAttribute("colspan")).toBe("4");
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
});
