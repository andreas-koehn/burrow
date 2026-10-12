import { describe, it, expect, vi, afterEach } from "vitest";
import { act, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { Route, Routes } from "react-router-dom";
import ServiceDetail from "@/pages/ServiceDetail";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";

function mount() {
  return renderApp(
    <Routes>
      <Route path="/services/:id" element={<ServiceDetail />} />
    </Routes>,
    "/services/svc_ai001",
  );
}

function mountAt(path: string) {
  return renderApp(
    <Routes>
      <Route path="/services/:id" element={<ServiceDetail />} />
    </Routes>,
    path,
  );
}

describe("ServiceDetail page", () => {
  it.each([
    ["svc_ai001", "Online", "status-online"],
    ["svc_graf01", "Offline", "status-offline"],
    ["svc_lms001", "Never connected", "status-never"],
  ])("writes the status of %s out as %s", async (id, label, kind) => {
    mountAt(`/services/${id}`);
    const badge = await screen.findByText(label);
    expect(badge).toHaveClass("badge", kind);
    expect(badge).not.toHaveClass("muted");
    for (const old of ["idle", "connected"]) expect(screen.queryByText(old)).toBeNull();
  });

  it("falls back to connected on a relay that sends no status", async () => {
    server.use(http.get("/api/v1/services/:id", () => HttpResponse.json({
      id: "svc_old", name: "old", type: "http", slug: "old123", url: "https://tunnels.example.com/svc/old123/",
      access_mode: "open", api_key_header: "Authorization", gateway_only: false, connected: false,
      remote_port: 0, local_addr: "", api_key_count: 0, access_policy: [],
    })));
    mountAt("/services/svc_old");
    expect(await screen.findByText("Offline")).toHaveClass("badge", "status-offline");
  });

  it("shows the full URL in the meta strip with a copy button, no subdomain host", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    expect(screen.getByText("https://tunnels.example.com/svc/ai4m2q/")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Copy URL https://tunnels.example.com/svc/ai4m2q/" }),
    ).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/ai4m2q\.tunnels\.example\.com/);
  });

  it("links to the traffic of this service", async () => {
    mount();
    expect(await screen.findByRole("link", { name: "View traffic" })).toHaveAttribute("href", "/traffic?service=svc_ai001");
  });

  it("an HTTP service that backs no provider links to its captured requests", async () => {
    mountAt("/services/svc_web01");
    expect(await screen.findByRole("link", { name: "Captured requests" })).toHaveAttribute("href", "/gateway/requests/svc_web01");
    expect(screen.getByRole("link", { name: "View traffic" })).toBeInTheDocument();
  });

  it("a provider's service has no such link: its requests are on the Requests page", async () => {
    mount();
    await screen.findByRole("link", { name: "View traffic" });
    await waitFor(() => expect(screen.queryByRole("link", { name: "Captured requests" })).toBeNull());
  });

  it("a service that is not HTTP has no captured requests to link to", async () => {
    const tcp = db.services.find((s) => s.type !== "http" && s.type !== "direct")!;
    mountAt(`/services/${tcp.id}`);
    await screen.findByRole("link", { name: "View traffic" });
    expect(screen.queryByRole("link", { name: "Captured requests" })).toBeNull();
  });

  it("the title is the service name", async () => {
    mount();
    expect(await screen.findByRole("heading", { level: 1, name: "ollama" })).toBeInTheDocument();
    expect(screen.queryByText(/Service ·/)).toBeNull();
  });

  it("a service that backs a provider links to it", async () => {
    mount();
    expect(await screen.findByRole("link", { name: "Provider: ollama" })).toHaveAttribute("href", "/gateway/providers/ollama");
  });

  it("names the provider as the gateway does, not by its slug", async () => {
    const provider = db.aiProviders.find((p) => p.slug === "ollama")!;
    provider.name = "Ollama on the GPU box";
    mount();
    expect(await screen.findByRole("link", { name: "Provider: Ollama on the GPU box" }))
      .toHaveAttribute("href", "/gateway/providers/ollama");
  });

  it("falls back to the slug where the providers cannot be listed", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    mount();
    expect(await screen.findByRole("link", { name: "Provider: ollama" })).toHaveAttribute("href", "/gateway/providers/ollama");
  });

  it("a service that backs no provider names none", async () => {
    const { qc } = mountAt("/services/svc_web01");
    await screen.findByRole("heading", { level: 1, name: "web" });
    await waitFor(() => expect(qc.getQueryState(["ai", "providers"])?.status).toBe("success"));
    expect(screen.queryByRole("link", { name: /^Provider:/ })).toBeNull();
  });

  it("shows who serves it: the client of the live session, linked", async () => {
    mount();
    const panel = await screen.findByRole("region", { name: "Client" });
    expect(within(panel).getByRole("status")).toHaveTextContent("Online · Served by office-box-1");
    expect(within(panel).getByRole("link", { name: "office-box-1" })).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });

  it("offline: says who served it last and since when", async () => {
    const svc = db.services.find((x) => x.id === "svc_graf01")!;
    svc.last_seen = new Date(Date.now() - 3 * 24 * 3600 * 1000).toISOString();
    svc.last_client = "koehn-laptop";
    mountAt("/services/svc_graf01");
    const panel = await screen.findByRole("region", { name: "Client" });
    // The first status line of the panel; the commands below have one of their own, for what was copied.
    expect(within(panel).getAllByRole("status")[0]).toHaveTextContent("Offline · last served by koehn-laptop 3 days ago");
    // Nobody serves it now: there is no session to link.
    expect(within(panel).queryByRole("link", { name: "koehn-laptop" })).toBeNull();
  });

  it("offline: shows the command that starts it, with the port it was last served from", async () => {
    mountAt("/services/svc_graf01");
    const panel = await screen.findByRole("region", { name: "Client" });
    expect(within(panel).getByRole("group", { name: "Run command" })).toHaveTextContent(/^burrow http 3001 --name grafana$/);
    // The one line: installing and signing in are on the page the link opens.
    expect(within(panel).queryByRole("group", { name: "Install command" })).toBeNull();
    expect(within(panel).queryByRole("group", { name: "Sign-in command" })).toBeNull();
    expect(within(panel).getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect?service=svc_graf01");
  });

  it("never connected: shows the command too, without a client to name", async () => {
    mountAt("/services/svc_lms001");
    const panel = await screen.findByRole("region", { name: "Client" });
    expect(within(panel).getAllByRole("status")[0]).toHaveTextContent(/^Never connected$/);
    // It backs a provider and no address is known: the port a model server listens on, as the provider page says.
    expect(within(panel).getByRole("group", { name: "Run command" })).toHaveTextContent(/^burrow http 11434 --name lmstudio$/);
    expect(within(panel).getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect?service=svc_lms001");
  });

  it("an online service shows no command", async () => {
    mount();
    const panel = await screen.findByRole("region", { name: "Client" });
    expect(within(panel).queryByRole("group", { name: "Run command" })).toBeNull();
    expect(within(panel).queryByRole("link", { name: "Connect a client" })).toBeNull();
    expect(panel).not.toHaveTextContent("burrow http");
  });

  it("a TCP service that is offline gets no burrow http command, and says why", async () => {
    const svc = db.services.find((x) => x.id === "svc_pg001")!;
    svc.connected = false;
    mountAt("/services/svc_pg001");
    const panel = await screen.findByRole("region", { name: "Client" });
    expect(within(panel).queryByRole("group", { name: "Run command" })).toBeNull();
    expect(panel).toHaveTextContent("This is not an HTTP service: burrow http does not serve it.");
    expect(within(panel).getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect?service=svc_pg001");
  });

  it("the hidden service of a hosted provider has no Client panel: no client serves it", async () => {
    mountAt("/services/prov-zai");
    await screen.findByRole("heading", { level: 1, name: "zai" });
    await screen.findByRole("tab", { name: "Access" });
    expect(screen.queryByRole("region", { name: "Client" })).toBeNull();
    expect(screen.queryByText(/burrow http/)).toBeNull();
    expect(screen.queryByRole("link", { name: "Connect a client" })).toBeNull();
  });

  describe("while nobody serves it", () => {
    afterEach(() => {
      vi.useRealTimers();
      server.events.removeAllListeners();
    });

    /** How often the service itself was asked for. */
    function countAsks(id: string) {
      const asked: string[] = [];
      server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
      return () => asked.filter((p) => p === `/api/v1/services/${id}`).length;
    }

    it("asks again until a client serves it, says Online by itself, and then asks no more", async () => {
      // Time moves on by itself too, so the first load and findBy keep working.
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const asks = countAsks("svc_graf01");
      mountAt("/services/svc_graf01");
      const panel = await screen.findByRole("region", { name: "Client" });
      const line = within(panel).getAllByRole("status")[0]!;
      expect(line).toHaveTextContent(/^Offline · last served by office-box-1/);
      expect(within(panel).getByRole("group", { name: "Run command" })).toBeInTheDocument();
      // Still offline: it is asked for again, every two seconds.
      const first = asks();
      await act(() => vi.advanceTimersByTimeAsync(2_100));
      await waitFor(() => expect(asks()).toBe(first + 1));
      expect(line).toHaveTextContent(/^Offline/);

      // The reader runs the command on the machine.
      db.services.find((x) => x.id === "svc_graf01")!.connected = true;
      await act(() => vi.advanceTimersByTimeAsync(2_100));
      // The same line, so the change is announced; the command is gone.
      await waitFor(() => expect(line).toHaveTextContent("Online · Served by office-box-1"));
      expect(line).toHaveAttribute("role", "status");
      expect(within(panel).queryByRole("group", { name: "Run command" })).toBeNull();
      expect(within(panel).queryByRole("link", { name: "Connect a client" })).toBeNull();

      const online = asks();
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(asks()).toBe(online);
    });

    it("does not ask again for a service that is online", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const asks = countAsks("svc_ai001");
      mount();
      await screen.findByRole("region", { name: "Client" });
      const first = asks();
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(asks()).toBe(first);
    });

    it("stops asking when the page is left", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const asks = countAsks("svc_graf01");
      const view = mountAt("/services/svc_graf01");
      await screen.findByRole("region", { name: "Client" });
      view.unmount();
      const left = asks();
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(asks()).toBe(left);
    });

    const NOTE = "This service could not be read again: the page may be out of date and no longer updates by itself.";

    it("an answer that fails leaves the page as it is, says so, and ends the asking", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      mountAt("/services/svc_graf01");
      const panel = await screen.findByRole("region", { name: "Client" });
      let failed = 0;
      server.use(http.get("/api/v1/services/:id", () => { failed++; return HttpResponse.json({ error: "boom" }, { status: 500 }); }));
      await act(() => vi.advanceTimersByTimeAsync(2_100));
      const note = await screen.findByText(NOTE);
      // A warning that is announced, not small print.
      expect(note.closest("[role=status]")).toHaveClass("notice-inline", "warn");
      // The service read before stays on the page, with its tabs: no error page in its place.
      expect(screen.getByRole("heading", { level: 1, name: "grafana" })).toBeInTheDocument();
      expect(screen.getByRole("tab", { name: "Access" })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
      expect(within(panel).getAllByRole("status")[0]).toHaveTextContent(/^Offline/);
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(failed).toBe(1);
    });

    it("a service that is gone when asked again gives way to the error view", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      mountAt("/services/svc_graf01");
      await screen.findByRole("region", { name: "Client" });
      let asked = 0;
      server.use(http.get("/api/v1/services/:id", () => { asked++; return HttpResponse.json({ error: "service not found" }, { status: 404 }); }));
      await act(() => vi.advanceTimersByTimeAsync(2_100));
      expect(await screen.findByRole("alert")).toHaveTextContent("service not found");
      // Nothing of the service is left to act on.
      expect(screen.queryByRole("region", { name: "Client" })).toBeNull();
      expect(screen.queryByRole("tab", { name: "Access" })).toBeNull();
      expect(screen.queryByRole("heading", { level: 1, name: "grafana" })).toBeNull();
      expect(screen.queryByText(NOTE)).toBeNull();
      expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(asked).toBe(1);
    });

    it("an expired session met when asking again has the session asked about", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const { qc } = mountAt("/services/svc_graf01");
      await screen.findByRole("region", { name: "Client" });
      const invalidate = vi.spyOn(qc, "invalidateQueries");
      let asked = 0;
      server.use(http.get("/api/v1/services/:id", () => { asked++; return HttpResponse.json({ error: "unauthorized" }, { status: 401 }); }));
      await act(() => vi.advanceTimersByTimeAsync(2_100));
      await waitFor(() => expect(invalidate).toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["me"] })));
      // Until the app's own handling takes over the page stays, and says that it is out of date.
      expect(screen.getByRole("heading", { level: 1, name: "grafana" })).toBeInTheDocument();
      expect(screen.getByText(NOTE)).toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(asked).toBe(1);
    });

    it("any other failure does not ask about the session", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const { qc } = mountAt("/services/svc_graf01");
      await screen.findByRole("region", { name: "Client" });
      const invalidate = vi.spyOn(qc, "invalidateQueries");
      server.use(http.get("/api/v1/services/:id", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
      await act(() => vi.advanceTimersByTimeAsync(2_100));
      await screen.findByText(NOTE);
      expect(invalidate).not.toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["me"] }));
    });

    it("says so for the hidden service of a hosted provider too, which has no Client panel", async () => {
      const { qc } = mountAt("/services/prov-zai");
      await screen.findByRole("heading", { level: 1, name: "zai" });
      expect(screen.queryByText(NOTE)).toBeNull();
      server.use(http.get("/api/v1/services/:id", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
      // It is not asked for by itself; a change made on the page reads it again.
      await act(() => qc.invalidateQueries({ queryKey: ["service", "prov-zai"] }));
      // It never updated by itself, so the note does not say that this has stopped.
      expect(await screen.findByText("This service could not be read again: the page may be out of date.")).toBeInTheDocument();
      expect(screen.queryByText(/no longer updates by itself/)).toBeNull();
      expect(screen.getByRole("heading", { level: 1, name: "zai" })).toBeInTheDocument();
      expect(screen.queryByRole("region", { name: "Client" })).toBeNull();
    });
  });

  it("a service that cannot be loaded at all still shows the error with Retry", async () => {
    server.use(http.get("/api/v1/services/:id", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mountAt("/services/svc_graf01");
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Client" })).toBeNull();
  });

  it("a name that cannot stand in a command is never put into one", async () => {
    const svc = db.services.find((x) => x.id === "svc_graf01")!;
    svc.name = "My Grafana; reboot";
    mountAt("/services/svc_graf01");
    const panel = await screen.findByRole("region", { name: "Client" });
    expect(within(panel).queryByRole("group", { name: "Run command" })).toBeNull();
    expect(panel).not.toHaveTextContent("reboot");
    expect(within(panel).getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect?service=svc_graf01");
  });

  it("shows three tabs: Access, API keys, Upstream key", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    const tablist = screen.getByRole("tablist");
    const tabs = within(tablist).getAllByRole("tab");
    const labels = tabs.map((t) => t.textContent?.trim());
    expect(labels).toContain("Access");
    expect(labels).toContain("API keys");
    expect(labels).toContain("Upstream key");
  });

  it("Upstream key tab renders the binding fields when a binding exists", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    // Switch to Upstream key tab
    const upstreamTab = screen.getByRole("tab", { name: /upstream key/i });
    await userEvent.click(upstreamTab);
    // The binding for svc_ai001 has header_name=Authorization and header_format=Bearer {key}
    const headerNameInput = await screen.findByLabelText(/header name/i);
    expect((headerNameInput as HTMLInputElement).value).toBe("Authorization");
    const headerFormatInput = screen.getByLabelText(/header format/i);
    expect((headerFormatInput as HTMLInputElement).value).toBe("Bearer {key}");
  });

  // P5.4 — #upstream-key hash deep-link opens Upstream-key tab directly
  it("P5.4: #upstream-key hash makes Upstream-key tab active on load", async () => {
    renderApp(
      <Routes>
        <Route path="/services/:id" element={<ServiceDetail />} />
      </Routes>,
      "/services/svc_ai001#upstream-key",
    );
    await screen.findByRole("heading", { name: /ollama/i });
    // The Upstream key tab should be selected (aria-selected="true")
    const upstreamTab = screen.getByRole("tab", { name: /upstream key/i });
    expect(upstreamTab).toHaveAttribute("aria-selected", "true");
    // And its panel content should be visible (binding fields rendered immediately)
    const headerNameInput = await screen.findByLabelText(/header name/i);
    expect(headerNameInput).toBeInTheDocument();
  });

  // The provider page links here for the keys a service holds from before gateway keys.
  it("#api-keys opens the API keys tab", async () => {
    mountAt("/services/svc_ai001#api-keys");
    await screen.findByRole("heading", { name: /ollama/i });
    expect(screen.getByRole("tab", { name: "API keys" })).toHaveAttribute("aria-selected", "true");
    const table = await screen.findByRole("table", { name: "API keys" });
    expect(await within(table).findByText("prod")).toBeInTheDocument();
  });

  it("API keys tab of a service that backs a provider: says what the keys are for and points models to a gateway key", async () => {
    mountAt("/services/svc_ai001#api-keys");
    await screen.findByRole("heading", { name: /ollama/i });
    const link = await screen.findByRole("link", { name: "gateway key" });
    expect(link).toHaveAttribute("href", "/gateway/keys");
    expect(link.closest("p")).toHaveTextContent(
      "Apps that call this service send one of these keys. For models, use a gateway key instead.",
    );
  });

  it("API keys tab of any other service: only what the keys are for", async () => {
    const { qc } = mountAt("/services/svc_graf01#api-keys");
    await screen.findByRole("heading", { name: /grafana/i });
    // The providers are known, so the second sentence is not just late.
    await waitFor(() => expect(qc.getQueryState(["ai", "providers"])?.status).toBe("success"));
    expect(screen.getByText(/Apps that call this service send one of these keys\./).textContent)
      .toBe("Apps that call this service send one of these keys.");
    expect(screen.queryByRole("link", { name: "gateway key" })).toBeNull();
  });

  it("API keys tab without the permission to read the keys: no sentence about keys that are not listed", async () => {
    db.me = { ...db.me, role: "user" };
    server.use(http.get("/api/v1/services/:id/api-keys", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    const { qc } = mountAt("/services/svc_ai001#api-keys");
    await screen.findByRole("heading", { name: /ollama/i });
    expect(await screen.findByText("Managing keys needs the services:configure permission.")).toBeInTheDocument();
    await waitFor(() => expect(qc.getQueryState(["service-api-keys", "svc_ai001"])?.status).toBe("error"));
    await waitFor(() => expect(qc.getQueryState(["ai", "providers"])?.status).toBe("success"));
    expect(screen.queryByText(/send one of these keys/)).toBeNull();
    expect(screen.queryByRole("link", { name: "gateway key" })).toBeNull();
  });

  it("a fragment that names no tab opens Access", async () => {
    mountAt("/services/svc_ai001#nope");
    await screen.findByRole("heading", { name: /ollama/i });
    expect(screen.getByRole("tab", { name: "Access" })).toHaveAttribute("aria-selected", "true");
  });

  it("Upstream key tab of a service that backs no provider: Providers leads to the steps that make one", async () => {
    mountAt("/services/svc_web01#upstream-key");
    await screen.findByRole("heading", { name: "web" });
    const link = await screen.findByRole("link", { name: "Providers" });
    expect(link).toHaveAttribute("href", "/gateway/providers?new=local");
    // It does not claim the service is listed there already.
    expect(link.closest("p")).toHaveTextContent(
      "This service can be used as a model provider — an admin can register it under Providers.",
    );
  });

  it("Upstream key tab of a service that backs a provider: nothing about registering it", async () => {
    mountAt("/services/svc_ai001#upstream-key");
    await screen.findByRole("heading", { name: "ollama" });
    const panel = screen.getByRole("tabpanel");
    await within(panel).findByLabelText(/header name/i);
    expect(panel).not.toHaveTextContent(/can be used as a model provider/);
    expect(within(panel).queryByRole("link", { name: "Providers" })).toBeNull();
  });

  it("renders the API keys table only in the API keys tab (C7)", async () => {
    mount();
    // svc_ai001 is in api_key mode, the mode that used to embed the key list.
    await screen.findByRole("radiogroup", { name: "Access mode" });
    expect(screen.getByRole("radio", { name: "API key" })).toBeChecked();
    expect(screen.getByLabelText("API key header")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New key" })).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: "API keys" }));
    expect(await screen.findByRole("button", { name: "New key" })).toBeInTheDocument();
  });

  it("titles a service without a name by its id", async () => {
    const svc = db.services.find((s) => s.id === "svc_graf01")!;
    const name = svc.name;
    svc.name = "";
    try {
      mountAt("/services/svc_graf01");
      expect(await screen.findByRole("heading", { level: 1, name: "svc_graf01" })).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Edit URL" }));
      expect(screen.getByRole("dialog", { name: "Change URL · svc_graf01" })).toBeInTheDocument();
    } finally {
      svc.name = name;
    }
  });

  it("changes the slug and shows the new URL", async () => {
    mountAt("/services/svc_web01");
    await userEvent.click(await screen.findByRole("button", { name: "Edit URL" }));
    const dialog = await screen.findByRole("dialog", { name: /change url/i });
    expect(within(dialog).getByText(/old URL stops working immediately/i)).toBeInTheDocument();
    const slug = within(dialog).getByLabelText("URL slug");
    await userEvent.clear(slug);
    await userEvent.type(slug, "web");
    await userEvent.click(within(dialog).getByRole("button", { name: "Change URL" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText("https://tunnels.example.com/svc/web/")).toBeInTheDocument();
  });

  it("keeps the dialog open and shows the server error when the slug is taken", async () => {
    mountAt("/services/svc_web01");
    await userEvent.click(await screen.findByRole("button", { name: "Edit URL" }));
    const dialog = await screen.findByRole("dialog", { name: /change url/i });
    const slug = within(dialog).getByLabelText("URL slug");
    await userEvent.clear(slug);
    await userEvent.type(slug, "ai4m2q"); // slug of svc_ai001 in the fixtures
    await userEvent.click(within(dialog).getByRole("button", { name: "Change URL" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("slug already in use");
  });

  it("has no custom-domains tab", async () => {
    mountAt("/services/svc_web01");
    await screen.findByRole("tab", { name: "Access" });
    expect(screen.queryByRole("tab", { name: /custom domains/i })).toBeNull();
  });

  it.each([
    ["svc_web01", "Open"],
    ["svc_graf01", "Burrow login"],
  ])("warns in a notice of its own that %s (%s) shares the dashboard's address", async (id) => {
    mountAt(`/services/${id}`);
    const note = await within(await screen.findByRole("tabpanel")).findByRole("note");
    expect(note).toHaveTextContent(
      /^This service shares the dashboard's address\. A page it serves can act as the signed-in dashboard user\. Only publish apps you trust\.$/,
    );
    // The warning style of the email notice, not grey small print.
    expect(note).toHaveClass("notice-inline", "warn");
    expect(note).not.toHaveClass("muted");
  });

  it("does not warn about the shared address for an API-key service", async () => {
    mount();
    await screen.findByRole("radiogroup", { name: "Access mode" });
    expect(screen.queryByText(/shares the dashboard's address/)).toBeNull();
  });

  it("does not warn about the shared address for a service without a public URL", async () => {
    const svc = db.services.find((x) => x.id === "svc_web01")!;
    svc.gateway_only = true;
    mountAt("/services/svc_web01");
    await screen.findByRole("radiogroup", { name: "Access mode" });
    expect(screen.queryByText(/shares the dashboard's address/)).toBeNull();
  });

  it("the gateway-only switch is the first block of the Access tab", async () => {
    mountAt("/services/svc_ai001");
    const panel = await screen.findByRole("tabpanel");
    const toggle = await within(panel).findByRole("switch", { name: "Reachable through the AI gateway only" });
    const modes = within(panel).getByRole("radiogroup", { name: "Access mode" });
    expect(toggle.compareDocumentPosition(modes) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(panel).getByText(/Closes the direct address and custom domains/)).toBeInTheDocument();
  });

  it("the gateway-only switch is not shown on the other tabs", async () => {
    const { qc } = mountAt("/services/svc_ai001#api-keys");
    await screen.findByRole("heading", { name: "ollama" });
    await waitFor(() => expect(qc.getQueryState(["ai", "providers"])?.status).toBe("success"));
    expect(screen.queryByRole("switch", { name: /gateway only/i })).toBeNull();
  });

  it("lets the owner close a service to everything but the AI gateway", async () => {
    mountAt("/services/svc_ai001");
    const toggle = await screen.findByRole("switch", { name: "Reachable through the AI gateway only" });
    expect(toggle).toHaveAttribute("aria-checked", "false");
    await userEvent.click(toggle);
    await waitFor(() => expect(toggle).toHaveAttribute("aria-checked", "true"));
    expect(screen.queryByRole("button", { name: /copy url/i })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit URL" })).toBeNull();
    expect(screen.getByText("No public URL: this service answers through the AI gateway only.")).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/\/svc\//);
    db.services.find((x) => x.id === "svc_ai001")!.gateway_only = false;
  });

  it("does not offer the switch for a service that backs no provider", async () => {
    mountAt("/services/svc_web01");
    await screen.findByRole("tab", { name: "Access" });
    expect(screen.queryByRole("switch", { name: /gateway only/i })).toBeNull();
  });

  it("keeps the switch for a gateway-only service whose provider is gone", async () => {
    const svc = db.services.find((x) => x.id === "svc_web01")!;
    svc.gateway_only = true;
    try {
      mountAt("/services/svc_web01");
      const toggle = await screen.findByRole("switch", { name: "Reachable through the AI gateway only" });
      expect(toggle).toHaveAttribute("aria-checked", "true");
      await userEvent.click(toggle);
      // Switched off, and with no provider behind it the switch goes away again.
      await waitFor(() => expect(screen.queryByRole("switch", { name: /gateway only/i })).toBeNull());
      expect(svc.gateway_only).toBe(false);
    } finally {
      svc.gateway_only = false;
    }
  });

  it("ties the explanation to the switch and announces a failed change", async () => {
    server.use(
      http.put("/api/v1/services/:id/gateway-only", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })),
    );
    mountAt("/services/svc_ai001");
    const toggle = await screen.findByRole("switch", { name: "Reachable through the AI gateway only" });
    expect(toggle).toHaveAccessibleDescription(/Closes the direct address and custom domains/);
    await userEvent.click(toggle);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("forbidden");
    expect(toggle).toHaveAttribute("aria-checked", "false");
  });
});
