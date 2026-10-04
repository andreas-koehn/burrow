import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { render } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";

function PathProbe() {
  return <div data-testid="path">{useLocation().pathname}</div>;
}

function renderAt(route: string) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider><QueryClientProvider client={qc}><MemoryRouter initialEntries={[route]}><App /><PathProbe /></MemoryRouter></QueryClientProvider></ThemeProvider>,
  );
}

describe("v0.5.0 routes", () => {
  it.each([
    ["/connection-logs",            /^Connection logs$/i],
    ["/settings/retention",         /^Retention & compliance$/i],
    ["/settings/database",          /^Database backend$/i],
  ])("%s resolves to its page heading", async (path, heading) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  it("redirects the retired custom-domain routes", async () => {
    renderAt("/settings/custom-domains");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
  });

  it("redirects a service's domains route to the service", async () => {
    renderAt("/services/svc_web01/domains");
    expect(await screen.findByRole("tab", { name: "Access" })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/services\/svc_web01$/);
  });
});
