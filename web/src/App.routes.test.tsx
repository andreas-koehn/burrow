import { describe, it, expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { render } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";

function renderAt(route: string) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider><QueryClientProvider client={qc}><MemoryRouter initialEntries={[route]}><App /></MemoryRouter></QueryClientProvider></ThemeProvider>,
  );
}

describe("App routes", () => {
  it("renders Roles at /roles", async () => {
    renderAt("/roles");
    expect(await screen.findByRole("heading", { name: /^Roles$/i })).toBeInTheDocument();
  });
  it("renders Settings at /settings", async () => {
    renderAt("/settings");
    expect(await screen.findByRole("heading", { name: /^Settings$/i })).toBeInTheDocument();
  });
  it("renders Clients at /clients", async () => {
    renderAt("/clients");
    expect(await screen.findByRole("heading", { name: /^Clients$/i })).toBeInTheDocument();
  });
  it("renders Connect at /clients/connect", async () => {
    renderAt("/clients/connect");
    expect(await screen.findByRole("heading", { name: /connect a client/i })).toBeInTheDocument();
  });
  it("shows admin nav links for an admin", async () => {
    renderAt("/account");
    expect(await screen.findByRole("link", { name: /^Roles$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^Settings$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^Clients$/i })).toBeInTheDocument();
  });

  // ---- v0.4.0 new routes ----
  it.each([
    ["/gateway/providers",             /^Providers$/i],
    ["/gateway/providers/ollama",      /^Provider · /i],
    ["/cache",                         /^Prompt cache$/i],
    ["/guardrails",                    /^Guardrails & redaction$/i],
    ["/inspector/svc_ai001",           /^Request inspector$/i],
    ["/cost",                          /^Cost & budgets$/i],
    ["/audit",                         /^Audit log$/i],
    ["/webhooks",                      /^Webhooks$/i],
    ["/account/automation",            /^Automation tokens$/i],
    ["/settings/backups",              /^Backup & restore$/i],
  ])("resolves %s to its page heading", async (path, heading) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  // v0.4.0 conditional nav: AI GATEWAY group appears when an api_key service exists.
  it("shows the AI GATEWAY nav group when an api_key service exists", async () => {
    renderAt("/account");
    expect(await screen.findByRole("link", { name: /^Providers$/i })).toHaveAttribute("href", "/gateway/providers");
    expect(screen.getByRole("link", { name: /^Cost & budgets$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^Prompt cache$/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^Guardrails$/i })).toBeInTheDocument();
  });

  it("/provisioning is unreachable (backend pending)", async () => {
    renderAt("/provisioning");
    await waitFor(() => {
      expect(screen.queryByRole("heading", { name: /provisioning keys/i })).toBeNull();
    });
  });
});
