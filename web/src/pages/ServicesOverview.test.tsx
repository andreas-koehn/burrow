import { describe, it, expect, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import { EMAIL_NOT_CONFIGURED } from "@/lib/copy";
import ServicesOverview from "./ServicesOverview";

const strip = () => screen.findByRole("list", { name: "Overview" });
const tiles = (el: HTMLElement) =>
  within(el).getAllByRole("listitem").map((t) => [
    t.querySelector(".label")?.textContent,
    t.querySelector(".value")?.textContent,
    within(t).getByRole("link").getAttribute("href"),
  ]);
const checklist = () => screen.findByRole("list", { name: "Set up Services" });
const stepStates = (list: HTMLElement) =>
  within(list).getAllByRole("listitem").map((li) => [
    within(li).getByRole("button").textContent,
    li.querySelector(".visually-hidden")?.textContent,
    within(li).getByRole("button").getAttribute("aria-expanded"),
  ]);

describe("ServicesOverview", () => {
  afterEach(() => resetDb());

  it("has the Overview heading and its subtitle", async () => {
    renderApp(<ServicesOverview />);
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    expect(screen.getByText("Machines, services and traffic on this relay.")).toBeInTheDocument();
  });

  it("shows four tiles, each a link to its page", async () => {
    renderApp(<ServicesOverview />);
    const el = await strip();
    // Seed: 1 client (10240 in / 4096 out), 4 services, 3 of them connected.
    await waitFor(() => expect(tiles(el)).toEqual([
      ["Clients online", "1", "/clients"],
      ["Services", "4", "/services"],
      ["Live now", "3", "/services?live=1"],
      ["Traffic", "10.0 KiB / 4.0 KiB", "/traffic"],
    ]));
    // Session totals of the clients connected right now, not a 24-hour figure.
    expect(within(el).getByText("in / out, connected clients")).toBeInTheDocument();
    expect(el.textContent).not.toMatch(/24h/);
    expect(el.textContent).not.toContain("`");
  });

  it("has no checklist once everything is set up; the strip stays", async () => {
    const { qc } = renderApp(<ServicesOverview />);
    const el = await strip();
    await waitFor(() => expect(tiles(el)[0]![1]).toBe("1"));
    // Every query the checklist waits for has answered by now.
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up Services" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Set up Services" })).toBeNull();
  });

  it("on a fresh relay lists the four setup steps in order, the first one open", async () => {
    db.tokens = []; db.clients = []; db.services = []; db.connectionLogs = [];
    renderApp(<ServicesOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Create a client token", "to do", "true"],
      ["Connect a client", "to do", "false"],
      ["Expose a service", "to do", "false"],
      ["Receive the first request", "to do", "false"],
    ]);
    expect(screen.getByText("0 of 4 done")).toBeInTheDocument();
    expect(within(list).getByRole("link", { name: "Create a token" })).toHaveAttribute("href", "/clients?tab=tokens");
    // The glossary sentences of the old explainer card live on as the steps' descriptions.
    expect(list.textContent).toMatch(/burrow connect/);
    expect(list.textContent).toMatch(/access mode/i);
  });

  it("follows the relay: with a token and a connected client the third step is the open one", async () => {
    db.services = []; db.connectionLogs = [];
    renderApp(<ServicesOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Create a client token", "done", "false"],
      ["Connect a client", "done", "false"],
      ["Expose a service", "to do", "true"],
      ["Receive the first request", "to do", "false"],
    ]);
    expect(within(list).getByRole("link", { name: "New service" })).toHaveAttribute("href", "/services?new=1");
  });

  it("each step leads to its page", async () => {
    db.tokens = []; db.clients = []; db.services = []; db.connectionLogs = [];
    const { default: userEvent } = await import("@testing-library/user-event");
    renderApp(<ServicesOverview />);
    const list = await checklist();
    for (const name of ["Connect a client", "Expose a service", "Receive the first request"]) {
      await userEvent.click(within(list).getByRole("button", { name }));
    }
    expect(within(list).getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual([
      "/clients?tab=tokens", "/clients/connect", "/services?new=1", "/traffic",
    ]);
  });

  it("with services but no traffic yet only the last step is open", async () => {
    db.connectionLogs = [];
    renderApp(<ServicesOverview />);
    const list = await checklist();
    expect(stepStates(list).map((s) => s[1])).toEqual(["done", "done", "done", "to do"]);
    expect(within(list).getByRole("link", { name: "Open traffic" })).toHaveAttribute("href", "/traffic");
  });

  it("shows the email relay notice once, with its link, when SMTP is not configured", async () => {
    renderApp(<ServicesOverview />);
    expect(await screen.findAllByText(EMAIL_NOT_CONFIGURED)).toHaveLength(1);
    expect(screen.getByRole("link", { name: /set up email/i })).toHaveAttribute("href", "/settings/email");
  });

  it("shows no email notice when SMTP is configured", async () => {
    server.use(http.get("/api/v1/settings", () => HttpResponse.json({ "smtp.host": "smtp.example.com" })));
    renderApp(<ServicesOverview />);
    const el = await strip();
    await waitFor(() => expect(tiles(el)[0]![1]).toBe("1"));
    expect(screen.queryByText(/email isn't set up/i)).toBeNull();
  });

  it("has no 'How Burrow works' card, no budget and no certificate notice", async () => {
    server.use(
      http.get("/api/v1/budgets", () => HttpResponse.json([
        { id: "b1", scope: "global", subject_id: "", daily_usd: 1, action_on_exceed: "alert_webhook", alert_webhook_id: null, current_usd: 5, exceeded: true },
      ])),
    );
    renderApp(<ServicesOverview />);
    await screen.findByText(EMAIL_NOT_CONFIGURED);
    expect(screen.queryByText(/how burrow works/i)).toBeNull();
    expect(document.querySelector(".home-explainer")).toBeNull();
    expect(screen.queryByText(/budget/i)).toBeNull();
    expect(screen.queryByText(/certificate/i)).toBeNull();
    expect(screen.queryByText(/AI cost/i)).toBeNull();
  });

  it("keeps the quick actions in the header", async () => {
    renderApp(<ServicesOverview />);
    await screen.findByRole("heading", { name: "Overview" });
    const header = document.querySelector(".page-header") as HTMLElement;
    expect(within(header).getByRole("link", { name: /connect a client/i })).toHaveAttribute("href", "/clients/connect");
    expect(within(header).getByRole("link", { name: /new service/i })).toHaveAttribute("href", "/services");
  });

  it("non-admin: no admin-only request, dashes for what they cannot see, no error notice", async () => {
    db.me = { ...db.me, role: "user" };
    db.services = [];
    const asked: string[] = [];
    const spy = (path: string) => http.get(`/api/v1/${path}`, () => { asked.push(path); return HttpResponse.json({ error: "admin required" }, { status: 403 }); });
    server.use(spy("clients"), spy("settings"), spy("connection-logs"), spy("budgets"));
    renderApp(<ServicesOverview />);
    const list = await checklist();
    const el = await strip();
    expect(tiles(el).map((t) => t[1])).toEqual(["—", "0", "0", "—"]);
    // The traffic log is admin-only, so its step is not theirs to tick off.
    expect(stepStates(list).map((s) => s[0])).toEqual(["Create a client token", "Connect a client", "Expose a service"]);
    expect(asked).toEqual([]);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/email isn't set up/i)).toBeNull();
  });

  it("services failing: the page says so and guesses no checklist", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderApp(<ServicesOverview />);
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load services/i);
    expect(screen.getByRole("link", { name: /connect a client/i })).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Set up Services" })).toBeNull();
  });

  it("someone without a token of their own on a relay that is set up: the token step counts as done", async () => {
    // GET /tokens lists only the caller's tokens; a second admin has none.
    db.tokens = [];
    const { qc } = renderApp(<ServicesOverview />);
    const el = await strip();
    await waitFor(() => expect(tiles(el)[0]![1]).toBe("1"));
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up Services" })).toBeNull();
  });

  it("no own token and no client, but a saved service: the token step is done all the same", async () => {
    db.tokens = []; db.clients = []; db.connectionLogs = [];
    db.services = db.services.slice(0, 1).map((s) => ({ ...s, connected: false }));
    renderApp(<ServicesOverview />);
    const list = await checklist();
    expect(stepStates(list).map((s) => [s[0], s[1]])).toEqual([
      ["Create a client token", "done"],
      ["Connect a client", "to do"],
      ["Expose a service", "done"],
      ["Receive the first request", "to do"],
    ]);
  });
});
