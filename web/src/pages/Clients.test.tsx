import { describe, it, expect, afterEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import { Routes, Route, useLocation } from "react-router-dom";
import Clients from "@/pages/Clients";

function LocationProbe() {
  const { pathname, search } = useLocation();
  return <div data-testid="location">{pathname + search}</div>;
}
const currentLocation = () => screen.getByTestId("location").textContent ?? "";
const mountAt = (route: string) => renderApp(<><Clients /><LocationProbe /></>, route);
const mount = () => mountAt("/clients");

describe("Clients overview", () => {
  it("lists connected clients with platform and traffic", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    expect(await screen.findByText("office-box-1")).toBeInTheDocument();
    const row = screen.getByText("office-box-1").closest("tr")!;
    expect(within(row).getByText(/linux/i)).toBeInTheDocument();
  });

  it("has a link to client detail", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    const row = (await screen.findByText("office-box-1")).closest("tr")!;
    // The "View" button link — exact text match to avoid collision with the services-count link
    const links = within(row).getAllByRole("link");
    const viewLink = links.find((l) => l.textContent?.trim() === "View");
    expect(viewLink).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });

  it("services-count badge links to client detail (P3B.1)", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    await screen.findByText("office-box-1");
    const link = screen.getByRole("link", { name: /view \d+ services for office-box-1/i });
    expect(link).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });
});

describe("Clients — Clients | Tokens tabs", () => {
  it("has Clients and Tokens tabs that follow the URL", async () => {
    mount();
    expect(await screen.findByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("table", { name: "Clients" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Tokens" }));
    expect(currentLocation()).toMatch(/[?&]tab=tokens/);
    expect(await screen.findByRole("table", { name: /tokens/i })).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "Clients" })).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: "Clients" }));
    expect(currentLocation()).toBe("/clients");
  });

  it("opens on the Tokens tab from the old link", async () => {
    mountAt("/clients?tab=tokens");
    expect(await screen.findByRole("tab", { name: "Tokens" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
  });

  it("treats ?tab=clients and an unknown tab as the clients list", async () => {
    const first = mountAt("/clients?tab=clients");
    expect(await screen.findByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
    first.unmount();
    mountAt("/clients?tab=nope");
    expect(await screen.findByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
  });

  it("names the other two kinds of credential on the Tokens tab", async () => {
    mountAt("/clients?tab=tokens");
    expect(await screen.findByRole("link", { name: /automation tokens/i })).toHaveAttribute("href", "/settings/automation");
    expect(screen.getByRole("link", { name: "Services" })).toHaveAttribute("href", "/services");
  });

  it("keeps one page header, with Connect a client as its action, on both tabs", async () => {
    mountAt("/clients?tab=tokens");
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expect(screen.getByRole("heading", { level: 1, name: "Clients" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect");
  });

  it("keeps the Tokens tab usable when the clients list is refused", async () => {
    server.use(http.get("/api/v1/clients", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load clients/i);
    await userEvent.click(screen.getByRole("tab", { name: "Tokens" }));
    expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("ties each panel to its tab, and Home/End reach the first and last tab", async () => {
    mount();
    const clients = await screen.findByRole("tab", { name: "Clients" });
    expect(screen.getByRole("tabpanel", { name: "Clients" })).toHaveAttribute("id", clients.getAttribute("aria-controls")!);
    clients.focus();
    await userEvent.keyboard("{End}");
    const tokens = screen.getByRole("tab", { name: "Tokens" });
    expect(tokens).toHaveFocus();
    expect(tokens).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel", { name: "Tokens" })).toBeInTheDocument();
    await userEvent.keyboard("{Home}");
    expect(screen.getByRole("tab", { name: "Clients" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
  });

  it("its links that look like buttons are links only: one tab stop each", async () => {
    mount();
    const row = (await screen.findByText("office-box-1")).closest("tr")!;
    for (const link of [screen.getByRole("link", { name: "Connect a client" }), within(row).getByRole("link", { name: "View" })]) {
      expect(link.querySelector("button")).toBeNull();
      expect(link).toHaveClass("btn");
    }
  });

  // The clients list is admin only: anyone else starts on their own tokens.
  describe("for a non-admin", () => {
    afterEach(() => server.events.removeAllListeners());

    it("opens on the Tokens tab without asking for the clients list", async () => {
      db.me.role = "user";
      const asked: string[] = [];
      server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
      server.use(http.get("/api/v1/clients", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
      mount();
      expect(await screen.findByRole("tab", { name: "Tokens" })).toHaveAttribute("aria-selected", "true");
      expect(await screen.findByRole("table", { name: "Tokens" })).toBeInTheDocument();
      expect(screen.queryByRole("alert")).toBeNull();
      expect(asked).not.toContain("/api/v1/clients");
      expect(currentLocation()).toBe("/clients");
    });

    it("can still open the Clients tab, which the URL then names", async () => {
      db.me.role = "user";
      server.use(http.get("/api/v1/clients", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
      mount();
      await userEvent.click(await screen.findByRole("tab", { name: "Clients" }));
      expect(currentLocation()).toBe("/clients?tab=clients");
      expect(screen.getByRole("tab", { name: "Clients" })).toHaveAttribute("aria-selected", "true");
      expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load clients/i);
      await userEvent.click(screen.getByRole("tab", { name: "Tokens" }));
      expect(currentLocation()).toBe("/clients");
    });
  });
});
