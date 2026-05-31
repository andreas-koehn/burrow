import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClientProvider, QueryClient } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import Tunnels from "./Tunnels";

class FakeES {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  readyState: number = FakeES.OPEN;
  onerror: ((e: any) => void) | null = null;
  listeners: Record<string, ((e: any) => void)[]> = {};
  constructor() { (FakeES as any).last = this; }
  addEventListener(t: string, fn: (e: any) => void) { (this.listeners[t] ||= []).push(fn); }
  removeEventListener(t: string, fn: (e: any) => void) {
    this.listeners[t] = (this.listeners[t] || []).filter((f) => f !== fn);
  }
  close() { this.readyState = FakeES.CLOSED; }
  emit(t: string) { (this.listeners[t] || []).forEach((fn) => fn({})); }
  triggerError() { if (this.onerror) this.onerror({}); }
}
(globalThis as any).EventSource = FakeES as any;

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={["/tunnels"]}>
      <QueryClientProvider client={qc}><Tunnels /></QueryClientProvider>
    </MemoryRouter>
  );
  return qc;
}

describe("Tunnels", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders rows with byte counters and refetches on an SSE message", async () => {
    let calls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      calls++;
      return new Response(JSON.stringify([{ id: "t1", name: "web", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:3000", bytes_in: 11, bytes_out: 22, connected: true }]), { status: 200 }) as any;
    });
    setup();
    expect(await screen.findByText("web")).toBeInTheDocument();
    // bytes_in: 11 B, bytes_out: 22 B — rendered with formatBytes in merged TRAFFIC cell
    expect(screen.getByTitle("In: 11 bytes")).toBeInTheDocument();
    expect(screen.getByTitle("Out: 22 bytes")).toBeInTheDocument();
    const before = calls;
    act(() => { (FakeES as any).last.emit("tunnels"); });
    await waitFor(() => expect(calls).toBeGreaterThan(before));
  });

  it("renders the data table using the design-system table.data class", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([{ id: "t1", name: "web", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:3000", bytes_in: 11, bytes_out: 22, connected: true }]), { status: 200 }) as any
    );
    setup();
    await screen.findByText("web");
    expect(screen.getByRole("table").className).toContain("data");
  });

  it("invalidates ['me'] when SSE stream closes (onerror + CLOSED)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("[]", { status: 200 }) as any
    );
    const qc = setup();
    // Wait for initial render
    await screen.findByText(/No live tunnels/i);

    // Spy on qc.invalidateQueries AFTER the query client is set up
    const invalidate = vi.spyOn(qc, "invalidateQueries");

    act(() => {
      const es: FakeES = (FakeES as any).last;
      es.readyState = FakeES.CLOSED;
      es.triggerError();
    });

    await waitFor(() => {
      expect(invalidate).toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["me"] }));
    });
  });

  it("http rows show the hostname (mono, copy) + Access badge + Configure; tcp rows do not", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "th1", name: "web", type: "http", remote_port: 0, local_addr: "127.0.0.1:3000", hostname: "k7p2qx.tunnels.example.com", access_mode: "open", bytes_in: 11, bytes_out: 22, connected: true },
        { id: "th2", name: "ai", type: "http", remote_port: 0, local_addr: "127.0.0.1:11434", hostname: "ai4m2q.tunnels.example.com", access_mode: "api_key", bytes_in: 0, bytes_out: 0, connected: true },
        { id: "tt1", name: "pg", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:5432", bytes_in: 0, bytes_out: 0, connected: true },
      ]), { status: 200 }) as any
    );
    setup();
    const webRow = (await screen.findByText("web")).closest("tr")!;
    expect(within(webRow).getByText("k7p2qx.tunnels.example.com")).toBeInTheDocument();
    expect(within(webRow).getByRole("button", { name: /copy hostname k7p2qx\.tunnels\.example\.com/i })).toBeInTheDocument();
    expect(within(webRow).getByText("Open")).toBeInTheDocument();

    const aiRow = screen.getByText("ai").closest("tr")!;
    expect(within(aiRow).getByText("API key")).toBeInTheDocument();

    const pgRow = screen.getByText("pg").closest("tr")!;
    expect(within(pgRow).getByText(":9000")).toBeInTheDocument();
    expect(within(pgRow).getByText("Open")).toBeInTheDocument();
    expect(within(pgRow).queryByRole("button", { name: /copy hostname/i })).toBeNull();
    expect(within(pgRow).queryByRole("button", { name: /configure/i })).toBeNull();

    // Configure on an http row opens the AccessModePanel.
    await userEvent.click(within(webRow).getByRole("button", { name: /configure/i }));
    expect(await screen.findByRole("radiogroup", { name: /access mode/i })).toBeInTheDocument();
  });

  it("HTTP tunnels show '—' in REMOTE, not ':0' (C6)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.includes("/tunnels")) {
        return new Response(JSON.stringify([
          { id: "t1", name: "ai", type: "http", remote_port: 0, local_addr: "mockoai:8081",
            bytes_in: 0, bytes_out: 0, connected: true, hostname: "abc.test.local" },
          { id: "t2", name: "echo", type: "tcp", remote_port: 9002, local_addr: "127.0.0.1:8082",
            bytes_in: 0, bytes_out: 0, connected: true },
        ]), { status: 200 }) as Response;
      }
      return new Response("[]", { status: 200 }) as Response;
    });
    setup();
    await waitFor(() => {
      expect(screen.queryByText(":0")).toBeNull();
      expect(screen.getByText("abc.test.local")).toBeInTheDocument();
      expect(screen.getByText(":9002")).toBeInTheDocument();
    });
  });

  it("does NOT invalidate ['me'] on transient SSE error while CONNECTING", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("[]", { status: 200 }) as any
    );
    const qc = setup();
    await screen.findByText(/No live tunnels/i);

    const invalidate = vi.spyOn(qc, "invalidateQueries");

    act(() => {
      const es: FakeES = (FakeES as any).last;
      es.readyState = FakeES.CONNECTING; // browser is retrying
      es.triggerError();
    });

    // Give it a tick to ensure no spurious invalidation
    await new Promise((r) => setTimeout(r, 10));
    expect(invalidate).not.toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["me"] }));
  });

  it("TRAFFIC column merges IN+OUT into one cell matching Clients page shape (S6)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "t1", name: "echo", type: "tcp", remote_port: 9002, local_addr: "127.0.0.1:8082",
          bytes_in: 1024, bytes_out: 2048, connected: true },
      ]), { status: 200 }) as Response,
    );
    setup();
    await waitFor(() => screen.getByText("echo"));
    // OLD shape was separate IN / OUT columns. New shape merges into TRAFFIC.
    expect(screen.queryByRole("columnheader", { name: /^in$/i })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: /^out$/i })).toBeNull();
    expect(screen.getByRole("columnheader", { name: /traffic/i })).toBeInTheDocument();
  });

  // P3A.1 — Tunnels explainer
  it("shows a live-connections explainer with a link to /services (role=note, NOT alert)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("[]", { status: 200 }) as any
    );
    setup();
    // The explainer text must be present
    const explainer = await screen.findByText(/live connections/i);
    expect(explainer).toBeInTheDocument();
    // It must contain (or be near) a link to /services
    const container = explainer.closest("[role='note']") ?? explainer.parentElement!;
    const link = container.querySelector("a[href='/services']");
    expect(link).not.toBeNull();
    // The explainer is NOT a role=alert region
    expect(screen.queryByRole("alert", { name: /live connections/i })).toBeNull();
  });

  // P3A.3 — Tunnel row → durable Service drill-down link
  it("http tunnel with service_id has an 'Open service' link to /services/<service_id>", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "th1", name: "web", type: "http", remote_port: 0, local_addr: "127.0.0.1:3000",
          hostname: "k7p2qx.tunnels.example.com", access_mode: "open",
          bytes_in: 0, bytes_out: 0, connected: true, service_id: "svc_web01" },
      ]), { status: 200 }) as any
    );
    setup();
    await screen.findByText("web");
    const link = screen.getByRole("link", { name: /open service web/i });
    expect(link).toHaveAttribute("href", "/services/svc_web01");
  });

  it("tcp tunnel without service_id does NOT render an 'Open service' link", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify([
        { id: "tt1", name: "pg", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:5432",
          bytes_in: 0, bytes_out: 0, connected: true },
      ]), { status: 200 }) as any
    );
    setup();
    await screen.findByText("pg");
    expect(screen.queryByRole("link", { name: /open service/i })).toBeNull();
  });
});
