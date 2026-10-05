import { describe, it, expect, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import { EMAIL_NOT_CONFIGURED } from "@/lib/copy";
import type { AiProvider, Budget } from "@/lib/contract";
import GatewayOverview from "./GatewayOverview";

function provider(over: Partial<AiProvider>): AiProvider {
  return {
    slug: "ollama", name: "ollama", kind: "tunnel", api_format: "openai", service_id: "svc_ai001",
    base_url: "https://tunnels.example.com/ai/ollama/v1", upstream_base_url: "", credential_slot: "",
    credential_present: false, billing: "metered", model_count: 0, model_alias: "fast", concrete_model: "llama3.1:8b",
    backend_type: "ollama", api_key_count: 1, requests_24h: 10, cache_hits_24h: 0, latency_p95_ms: 0, status: "Connected", client_session_id: "sess_1",
    ...over,
  };
}
const withProviders = (list: AiProvider[]) =>
  server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json(list)));
const withBudgets = (list: Partial<Budget>[]) =>
  server.use(http.get("/api/v1/budgets", () => HttpResponse.json(list.map((b, i) => ({
    id: `b${i}`, scope: "global", subject_id: "", daily_usd: 10, action_on_exceed: "alert_webhook",
    alert_webhook_id: null, current_usd: 0, exceeded: false, ...b,
  })))));

const strip = () => screen.findByRole("list", { name: "AI Gateway" });
const tiles = (el: HTMLElement) =>
  within(el).getAllByRole("listitem").map((t) => [
    t.querySelector(".label")?.textContent,
    t.querySelector(".value")?.textContent,
    within(t).getByRole("link").getAttribute("href"),
  ]);
const checklist = () => screen.findByRole("list", { name: "Set up the AI Gateway" });
const stepStates = (list: HTMLElement) =>
  within(list).getAllByRole("listitem").map((li) => [
    li.querySelector(".setup-step-title")?.textContent,
    li.querySelector(".visually-hidden")?.textContent,
    li.querySelector(".setup-step-title")?.getAttribute("aria-expanded"),
  ]);
const rows = () =>
  within(screen.getByRole("table", { name: "Providers" })).getAllByRole("row").slice(1)
    .map((r) => within(r).getAllByRole("cell").map((c) => c.textContent));

describe("GatewayOverview", () => {
  afterEach(() => resetDb());

  it("has the AI Gateway heading and a strip of linked tiles, without error rate or latency", async () => {
    renderApp(<GatewayOverview />);
    expect(await screen.findByRole("heading", { name: "AI Gateway" })).toBeInTheDocument();
    const el = await strip();
    // Seed: one provider with 1,024 requests; $1.23 today.
    await waitFor(() => expect(tiles(el)).toEqual([
      ["Requests 24h", "1,024", "/gateway/requests"],
      ["Cost 24h", "$1.23", "/gateway/cost"],
    ]));
    // The relay reports neither an error count nor a recorded latency per provider.
    expect(screen.queryByText(/error rate/i)).toBeNull();
    expect(screen.queryByText(/latency/i)).toBeNull();
    expect(document.body.textContent).not.toMatch(/NaN/);
  });

  it("leaves the cost tile out when the relay has no cost endpoint", async () => {
    server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({ error: "x" }, { status: 404 })));
    const { qc } = renderApp(<GatewayOverview />);
    const el = await strip();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(tiles(el).map((t) => t[0])).toEqual(["Requests 24h"]);
  });

  it("lists every provider with a link, whether it is reachable, and its requests", async () => {
    withProviders([
      provider({ slug: "ollama", name: "ollama", requests_24h: 1024 }),
      provider({ slug: "gone", name: "gone", status: "Offline", requests_24h: 0 }),
      provider({ slug: "slow", name: "slow", status: "Degraded", requests_24h: 3 }),
      provider({ slug: "zai", name: "zai", kind: "direct", status: "Connected", credential_present: true, requests_24h: 7 }),
      provider({ slug: "bare", name: "bare", kind: "direct", status: "Offline", credential_present: false, requests_24h: 0 }),
    ]);
    renderApp(<GatewayOverview />);
    await screen.findByRole("table", { name: "Providers" });
    expect(rows()).toEqual([
      ["ollama", "connected", "1,024"],
      ["gone", "client offline", "0"],
      ["slow", "degraded", "3"],
      ["zai", "ready", "7"],
      ["bare", "not configured", "0"],
    ]);
    expect(screen.getByRole("link", { name: "zai" })).toHaveAttribute("href", "/gateway/providers/zai");
    expect(screen.getByRole("columnheader", { name: "Requests (24h)" })).toBeInTheDocument();
  });

  it("has no checklist once a provider has a key and has served a request", async () => {
    const { qc } = renderApp(<GatewayOverview />);
    await screen.findByRole("table", { name: "Providers" });
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
  });

  it("with no provider at all: zero requests, and the checklist's first step instead of the list", async () => {
    withProviders([]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Add a provider", "to do", "true"],
      ["Create an API key", "to do", "false"],
      ["Send the first request", "to do", "false"],
    ]);
    expect(within(list).getByRole("link", { name: "Add a provider" })).toHaveAttribute("href", "/gateway/providers");
    expect(tiles(await strip())[0]).toEqual(["Requests 24h", "0", "/gateway/requests"]);
    expect(screen.queryByRole("table", { name: "Providers" })).toBeNull();
  });

  it("a provider without a key: the key step is open and leads to the first provider's page", async () => {
    withProviders([provider({ slug: "zai", name: "zai", api_key_count: 0, requests_24h: 0 }), provider({ slug: "b", name: "b", api_key_count: 0, requests_24h: 0 })]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Add a provider", "done", "false"],
      ["Create an API key", "to do", "true"],
      ["Send the first request", "to do", "false"],
    ]);
    expect(within(list).getByRole("link", { name: "Create an API key" })).toHaveAttribute("href", "/gateway/providers/zai");
  });

  it("a provider with a key but no request yet: the last step shows how to connect to the first provider", async () => {
    withProviders([provider({ slug: "zai", name: "zai", base_url: "https://b.example.com/ai/zai/v1", concrete_model: "glm-4", api_key_count: 1, requests_24h: 0 })]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list).map((s) => [s[1], s[2]])).toEqual([["done", "false"], ["done", "false"], ["to do", "true"]]);
    expect(within(list).getByRole("button", { name: "Copy base URL https://b.example.com/ai/zai/v1" })).toBeInTheDocument();
    expect(list.textContent).toContain("curl https://b.example.com/ai/zai/v1/chat/completions");
    expect(list.textContent).toContain('"model": "glm-4"');
  });

  it("a budget above 80 % of its limit is a notice that leads to the budgets", async () => {
    withBudgets([{ daily_usd: 10, current_usd: 8.5 }, { daily_usd: 10, current_usd: 8 }]);
    renderApp(<GatewayOverview />);
    expect(await screen.findByText("A budget is above 80 % of its daily limit.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /view budgets/i })).toHaveAttribute("href", "/gateway/cost");
    expect(screen.queryByText(/over (its|their) daily limit/)).toBeNull();
  });

  it("an exceeded budget is named as such", async () => {
    withBudgets([{ daily_usd: 1, current_usd: 5, exceeded: true }, { daily_usd: 1, current_usd: 2, exceeded: true }]);
    renderApp(<GatewayOverview />);
    expect(await screen.findByText("2 budgets are over their daily limit.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /view budgets/i })).toHaveAttribute("href", "/gateway/cost");
  });

  it("no budget notice at half the limit (seed)", async () => {
    const { qc } = renderApp(<GatewayOverview />);
    await strip();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByText(/budget/i)).toBeNull();
  });

  it("shows the same email relay notice as the Services Overview", async () => {
    renderApp(<GatewayOverview />);
    expect(await screen.findAllByText(EMAIL_NOT_CONFIGURED)).toHaveLength(1);
    expect(screen.getByRole("link", { name: /set up email/i })).toHaveAttribute("href", "/settings/email");
  });

  it("non-admin: no admin-only request and no error notice; setup starts at the key", async () => {
    db.me = { ...db.me, role: "user" };
    const asked: string[] = [];
    const spy = (path: string) => http.get(`/api/v1/${path}`, () => { asked.push(path); return HttpResponse.json({ error: "admin required" }, { status: 403 }); });
    server.use(spy("settings"), spy("budgets"), spy("clients"));
    withProviders([provider({ api_key_count: 0, requests_24h: 0 })]);
    const { qc } = renderApp(<GatewayOverview />);
    const list = await checklist();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(stepStates(list).map((s) => s[0])).toEqual(["Create an API key", "Send the first request"]);
    expect(asked).toEqual([]);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/email isn't set up/i)).toBeNull();
  });

  it("non-admin without a provider: told who can add one, no checklist", async () => {
    db.me = { ...db.me, role: "user" };
    withProviders([]);
    renderApp(<GatewayOverview />);
    expect(await screen.findByText(/An administrator can add one/)).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
  });

  it("a relay without the AI gateway says so instead of showing zeros", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json({ error: "x" }, { status: 404 })));
    renderApp(<GatewayOverview />);
    expect(await screen.findByText("AI gateway isn't available on this relay")).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "AI Gateway" })).toBeNull();
  });

  it("providers failing: an error with a retry, no figures and no checklist", async () => {
    server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderApp(<GatewayOverview />);
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't load providers/i);
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "AI Gateway" })).toBeNull();
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
  });
});
