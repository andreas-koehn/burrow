import { describe, it, expect, vi } from "vitest";
import { http, HttpResponse } from "msw";
import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { ThemeProvider } from "@/components/theme-provider";
import { CommandPalette } from "@/components/CommandPalette";
import { setCsrfCookie } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { holdsAny, ALL_PERMISSIONS, USER_PERMISSIONS } from "@/auth/permissions";

// Helper to render CommandPalette inside a router and query provider.
// Renders a "spy" route so we can detect navigation by checking current location.
function SpyHeading() {
  const loc = useLocation();
  return <div data-testid="spy-location">{loc.pathname}{loc.search}</div>;
}

interface RenderOptions {
  open?: boolean;
  /** What the caller may do; everything by default. */
  perms?: readonly string[];
  hasAiGateway?: boolean;
  onOpenChange?: (open: boolean) => void;
}

function renderPalette({
  open = true,
  perms = ALL_PERMISSIONS,
  hasAiGateway = true,
  onOpenChange = vi.fn(),
}: RenderOptions = {}) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/"]}>
          <Routes>
            <Route
              path="/*"
              element={
                <>
                  <SpyHeading />
                  <CommandPalette
                    open={open}
                    onOpenChange={onOpenChange}
                    can={(needs) => holdsAny(new Set(perms), needs)}
                    hasAiGateway={hasAiGateway}
                  />
                </>
              }
            />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { ...result, onOpenChange, qc };
}

describe("CommandPalette — P6A.1: destinations", () => {
  it("Cl-3: open=true renders a role=dialog element", () => {
    renderPalette();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("does not render when open=false", () => {
    renderPalette({ open: false });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("renders a search input", () => {
    renderPalette();
    expect(screen.getByRole("searchbox")).toBeInTheDocument();
  });

  it("lists the navigation description, grouped by workspace", () => {
    renderPalette();
    const rows = screen.getAllByRole("option").map((o) => o.textContent);
    // Services first, then the gateway, then Settings.
    expect(rows.slice(0, 5)).toEqual([
      "Services overviewServices", "ServicesServices", "ClientsServices", "TrafficServices",
      "AI Gateway overviewAI Gateway",
    ]);
    const groups = new Set(screen.getAllByRole("option").map((o) => o.querySelector(".shortcut")?.textContent));
    expect(groups).toEqual(new Set(["Services", "AI Gateway", "Settings"]));
  });

  // Both workspaces have an "Overview"; side by side the two rows would read the same.
  it("names the two overviews apart", async () => {
    renderPalette();
    const names = () => screen.getAllByRole("option").map((o) => o.querySelector("span")?.textContent);
    expect(names().filter((n) => /overview/i.test(n ?? ""))).toEqual(["Services overview", "AI Gateway overview"]);
    expect(names()).not.toContain("Overview");
    await userEvent.type(screen.getByRole("searchbox"), "overview");
    expect(names()).toEqual(["Services overview", "AI Gateway overview"]);
    fireEvent.click(screen.getByRole("option", { name: /^AI Gateway overview/ }));
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/gateway$/);
  });

  it("finds an overview by its full name", async () => {
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "ai gateway over");
    expect(screen.getAllByRole("option").map((o) => o.querySelector("span")?.textContent)).toEqual(["AI Gateway overview"]);
  });

  // Clients is in both sidebars and is still one page.
  it("lists Clients once, and opens the one page", async () => {
    renderPalette();
    const pages = () => screen.getAllByRole("option").filter((o) => o.querySelector("span")?.textContent === "Clients");
    expect(pages()).toHaveLength(1);
    await userEvent.type(screen.getByRole("searchbox"), "clients");
    expect(pages()).toHaveLength(1);
    fireEvent.click(pages()[0]);
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/clients$/);
  });

  it("typing 'cost' finds 'Cost & budgets' under 'AI Gateway' and navigates to its new path", async () => {
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "cost");
    const rows = screen.getAllByRole("option");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("Cost & budgets");
    expect(rows[0].querySelector(".shortcut")).toHaveTextContent("AI Gateway");
    fireEvent.click(rows[0]);
    expect(screen.getByTestId("spy-location")).toHaveTextContent("/gateway/cost");
  });

  // "Services" is a page and a workspace: the page of that name comes before the
  // pages that only sit in the workspace, so Enter opens it and not the Overview.
  it("typing 'services' puts the Services page first and Enter opens it", async () => {
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "services");
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options[0]).toBe("ServicesServices");
    expect(options).toContain("Services overviewServices");
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/services$/);
  });

  it("typing 'sett' lists the Settings pages (admin) and nothing from another workspace", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "sett");
    const rows = screen.getAllByRole("option");
    expect(rows.length).toBeGreaterThan(3);
    for (const r of rows) expect(r.querySelector(".shortcut")).toHaveTextContent("Settings");
    expect(screen.queryByRole("option", { name: /^overview/i })).not.toBeInTheDocument();
  });

  it("without AI access there is no gateway destination", () => {
    renderPalette({ perms: USER_PERMISSIONS, hasAiGateway: false });
    const groups = screen.getAllByRole("option").map((o) => o.querySelector(".shortcut")?.textContent);
    expect(groups).not.toContain("AI Gateway");
    expect(screen.queryByRole("option", { name: /providers/i })).toBeNull();
  });

  it("a non-admin sees no admin entry, only the Personal settings", () => {
    renderPalette({ perms: USER_PERMISSIONS });
    const settings = screen.getAllByRole("option")
      .filter((o) => o.querySelector(".shortcut")?.textContent === "Settings")
      .map((o) => o.querySelector("span")?.textContent);
    expect(settings).toEqual(["Profile & password", "Sessions", "Automation tokens", "Connected apps"]);
  });

  it("typing 'zzz' shows 'No matches'", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "zzz");
    expect(screen.getByText(/no matches/i)).toBeInTheDocument();
  });

  it("ArrowDown then Enter navigates to the focused item and calls onOpenChange(false)", async () => {
    const onOpenChange = vi.fn();
    renderPalette({ onOpenChange });

    // The onKeyDown handler is on the flex div wrapping the input and listbox.
    // The dialog-body > div is where we need to fire the event.
    const dialog = screen.getByRole("dialog");
    const keyTarget = dialog.querySelector(".dialog-body > div") as HTMLElement;
    expect(keyTarget).not.toBeNull();

    // Arrow down from first item (index 0 = Services overview) to second item (index 1).
    fireEvent.keyDown(keyTarget, { key: "ArrowDown" });
    // Press Enter to activate index 1.
    fireEvent.keyDown(keyTarget, { key: "Enter" });

    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("Enter on first item navigates and calls onOpenChange(false)", async () => {
    const onOpenChange = vi.fn();
    renderPalette({ onOpenChange });

    const dialog = screen.getByRole("dialog");
    const keyTarget = dialog.querySelector(".dialog-body > div") as HTMLElement;
    expect(keyTarget).not.toBeNull();

    // Enter on focusIdx=0 (first item = Services overview → "/")
    fireEvent.keyDown(keyTarget, { key: "Enter" });

    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("clicking an item calls onOpenChange(false)", async () => {
    const onOpenChange = vi.fn();
    renderPalette({ onOpenChange });
    const firstItem = screen.getAllByRole("option")[0];
    fireEvent.click(firstItem);
    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("excludes admin-only destinations without the permissions", async () => {
    renderPalette({ perms: USER_PERMISSIONS });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "users");
    // Users is admin-only — should not appear for non-admin
    await waitFor(() => {
      expect(screen.queryByRole("option", { name: /users/i })).not.toBeInTheDocument();
    });
  });

  it("includes admin-only destinations with the permissions", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "users");
    // The option accessible name includes the group label "Settings" too.
    expect(await screen.findByRole("option", { name: /users/i })).toBeInTheDocument();
  });
});

describe("CommandPalette — P6A.2: live entities (MSW seed)", () => {
  it("typing 'ollam' shows 'ollama — service' from MSW seed", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "ollam");
    // MSW seed has service named 'ollama' (svc_ai001)
    expect(await screen.findByRole("option", { name: /ollama.*service/i })).toBeInTheDocument();
  });

  it("typing 'office' shows 'office-box-1 — client' from MSW seed (admin)", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "office");
    // MSW seed has client with token_name 'office-box-1'
    expect(await screen.findByRole("option", { name: /office-box-1.*client/i })).toBeInTheDocument();
  });

  it("clients do not appear for non-admin (query disabled)", async () => {
    renderPalette({ perms: USER_PERMISSIONS });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "office");
    // Wait briefly; clients should never appear for non-admin
    await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
    expect(screen.queryByRole("option", { name: /office-box-1.*client/i })).not.toBeInTheDocument();
  });

  it("asks for providers, models and keys only with the permissions of their routes, whatever shows the gateway", async () => {
    const asked: string[] = [];
    for (const path of ["providers", "models", "keys"]) {
      server.use(http.get(`/api/v1/ai/${path}`, () => { asked.push(path); return HttpResponse.json([]); }));
    }
    // A role with no AI permission, in a relay where an http service is connected (hasAiGateway).
    renderPalette({ perms: ["audit:read"], hasAiGateway: true });
    await userEvent.type(screen.getByRole("searchbox"), "a");
    await act(async () => { await new Promise((r) => setTimeout(r, 100)); });
    expect(asked).toEqual([]);
  });

  it("grafana service appears when typing 'graf'", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "graf");
    expect(await screen.findByRole("option", { name: /grafana.*service/i })).toBeInTheDocument();
  });
});

describe("CommandPalette — providers, models and gateway keys", () => {
  const results = () => screen.getAllByRole("option").map((o) => o.textContent);
  const seenOllama = () => screen.findByRole("option", { name: /ollama.*service/i });

  it("finds a provider by name", async () => {
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    // Seed: a provider and a service, both called ollama. The provider comes first.
    await waitFor(() => expect(results()).toEqual(["ollama — provider", "ollama — service"]));
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/gateway\/providers\/ollama$/);
  });

  it("leaves out a service that is reachable through the AI gateway only: its provider stands for it", async () => {
    db.services.find((s) => s.id === "svc_ai001")!.gateway_only = true;
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    await waitFor(() => expect(results()).toEqual(["ollama — provider"]));
    // The services have answered too: another one is found.
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "a");
    expect(await screen.findByRole("option", { name: /grafana.*service/i })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /ollama.*service/i })).toBeNull();
    expect(screen.getByRole("option", { name: /ollama.*provider/i })).toBeInTheDocument();
  });

  // The service is left out because its provider is listed: so the provider answers to the service's name too.
  it("finds the provider of a gateway-only service by the service's name", async () => {
    const service = db.services.find((s) => s.id === "svc_ai001")!;
    service.gateway_only = true;
    service.name = "gpu-box";
    db.aiProviders.find((p) => p.service_id === "svc_ai001")!.name = "Office GPU";
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "gpu-box");
    await waitFor(() => expect(results()).toEqual(["Office GPU — provider"]));
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/gateway\/providers\/ollama$/);
  });

  it("does not answer to the name of a service that has a row of its own", async () => {
    db.services.find((s) => s.id === "svc_ai001")!.name = "gpu-box";
    db.aiProviders.find((p) => p.service_id === "svc_ai001")!.name = "Office GPU";
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "gpu-box");
    expect(await screen.findByRole("option", { name: "gpu-box — service" })).toBeInTheDocument();
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "office gpu");
    expect(await screen.findByRole("option", { name: "Office GPU — provider" })).toBeInTheDocument();
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "gpu-box");
    expect(results()).toEqual(["gpu-box — service"]);
  });

  it("keeps a gateway-only service findable when the providers cannot be read", async () => {
    db.services.find((s) => s.id === "svc_ai001")!.gateway_only = true;
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    expect(await seenOllama()).toBeInTheDocument();
    await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
    expect(results()).toEqual(["ollama — service"]);
  });

  it("keeps a gateway-only service findable for someone without the AI gateway", async () => {
    db.services.find((s) => s.id === "svc_ai001")!.gateway_only = true;
    renderPalette({ perms: USER_PERMISSIONS, hasAiGateway: false });
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    expect(await seenOllama()).toBeInTheDocument();
    expect(results()).toEqual(["ollama — service"]);
  });

  it("keeps a gateway-only service that no listed provider stands for", async () => {
    db.services.find((s) => s.id === "svc_ai001")!.gateway_only = true;
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json([{ slug: "zai", name: "zai", service_id: "prov-zai" }])));
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    expect(await seenOllama()).toBeInTheDocument();
    expect(results()).toEqual(["ollama — service"]);
  });

  it("opens the provider by its slug when the name differs", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json([{ slug: "local-llm", name: "Office GPU" }])));
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "office g");
    fireEvent.click(await screen.findByRole("option", { name: "Office GPU — provider" }));
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/gateway\/providers\/local-llm$/);
  });

  it("finds a model and a gateway key by name", async () => {
    const first = renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "burrow-si");
    fireEvent.click(await screen.findByRole("option", { name: "burrow-simple — model" }));
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/gateway\/models\?focus=burrow-simple$/);
    first.unmount();

    // Seed: one gateway key, called laptop.
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "lapt");
    fireEvent.click(await screen.findByRole("option", { name: "laptop — gateway key" }));
    expect(screen.getByTestId("spy-location")).toHaveTextContent(/^\/gateway\/keys$/);
  });

  it("links a model whose name needs escaping", async () => {
    server.use(http.get("/api/v1/ai/models", () => HttpResponse.json([{ name: "team a/fast&cheap" }])));
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "team a");
    fireEvent.click(await screen.findByRole("option", { name: "team a/fast&cheap — model" }));
    expect(screen.getByTestId("spy-location")).toHaveTextContent("/gateway/models?focus=team%20a%2Ffast%26cheap");
  });

  it("does not ask for providers when the user has no AI gateway", async () => {
    const asked: string[] = [];
    const count = (what: string) => http.get(`/api/v1/ai/${what}`, () => { asked.push(what); return HttpResponse.json([]); });
    server.use(count("providers"), count("models"), count("keys"));
    renderPalette({ perms: USER_PERMISSIONS, hasAiGateway: false });
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    // The services have answered, so everything the palette asks for has been asked.
    expect(await seenOllama()).toBeInTheDocument();
    await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
    expect(asked).toEqual([]);
    expect(results()).toEqual(["ollama — service"]);
  });

  it("asks for them only once the palette is open", async () => {
    const asked: string[] = [];
    const count = (what: string) => http.get(`/api/v1/ai/${what}`, () => { asked.push(what); return HttpResponse.json([]); });
    server.use(count("providers"), count("models"), count("keys"));
    renderPalette({ open: false });
    await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
    expect(asked).toEqual([]);
  });

  it("a failed lookup does not break the palette", async () => {
    server.use(http.get("/api/v1/ai/models", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderPalette();
    // Pages are listed as ever.
    expect(results()).toContain("ServicesServices");
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    await waitFor(() => expect(results()).toEqual(["ollama — provider", "ollama — service"]));
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "burrow");
    await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
    expect(screen.queryByRole("option", { name: /model$/ })).toBeNull();
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "lapt");
    expect(await screen.findByRole("option", { name: "laptop — gateway key" })).toBeInTheDocument();
  });

  it("an answer that is not a list adds no rows", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json({})), http.get("/api/v1/ai/keys", () => HttpResponse.json(null)));
    renderPalette();
    await userEvent.type(screen.getByRole("searchbox"), "ollama");
    await waitFor(() => expect(results()).toEqual(["ollama — service"]));
  });
});
