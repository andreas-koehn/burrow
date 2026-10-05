import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
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
    ["/traffic",                    /^Traffic$/i],
    ["/settings/retention",         /^Retention & compliance$/i],
    ["/settings/database",          /^Database backend$/i],
  ])("%s resolves to its page heading", async (path, heading) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
  });

  it("redirects the retired custom-domains route", async () => {
    renderAt("/settings/custom-domains");
    expect(await screen.findByRole("heading", { name: "General", level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/settings\/general$/);
  });

  it("keeps the hash of a service bookmark", async () => {
    renderAt("/services/svc_web01#upstream-key");
    expect(await screen.findByRole("tab", { name: "Access" })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/services\/svc_web01$/);
    const nav = within(screen.getByRole("navigation", { name: "Services" }));
    expect(nav.getByRole("link", { name: /^Services(,|$)/ })).toHaveAttribute("aria-current", "page");
  });

  it("shows the Settings sidebar on a settings page, with the page's entry current", async () => {
    renderAt("/settings/email");
    expect(await screen.findByRole("heading", { name: "Email", level: 1 })).toBeInTheDocument();
    const nav = within(await screen.findByRole("navigation", { name: "Settings" }));
    expect(nav.getByRole("link", { name: "Email" })).toHaveAttribute("aria-current", "page");
    expect(nav.getByRole("link", { name: "General" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("link", { name: /^Back to / })).toBeInTheDocument();
  });

  it("redirects a service's domains route to the service", async () => {
    renderAt("/services/svc_web01/domains");
    expect(await screen.findByRole("tab", { name: "Access" })).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/services\/svc_web01$/);
  });
});
