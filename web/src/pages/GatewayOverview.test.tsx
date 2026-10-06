import { describe, it, expect, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import { EMAIL_NOT_CONFIGURED } from "@/lib/copy";
import type { AiGatewayKey, AiModel, AiProvider, Budget } from "@/lib/contract";
import GatewayOverview from "./GatewayOverview";

function provider(over: Partial<AiProvider>): AiProvider {
  return {
    slug: "ollama", name: "ollama", kind: "tunnel", api_format: "openai", service_id: "svc_ai001",
    base_url: "https://tunnels.example.com/ai/ollama/v1", upstream_base_url: "", credential_slot: "",
    credential_present: false, billing: "metered", supports_responses: false, model_count: 0, model_alias: "fast", concrete_model: "llama3.1:8b",
    backend_type: "ollama", api_key_count: 1, requests_24h: 10, cache_hits_24h: 0, latency_p95_ms: 0, status: "Connected", client_session_id: "sess_1",
    ...over,
  };
}
const withProviders = (list: AiProvider[]) =>
  server.use(http.get("/api/v1/ai/providers", () => HttpResponse.json(list)));
const withModels = (list: AiModel[]) =>
  server.use(http.get("/api/v1/ai/models", () => HttpResponse.json(list)));
const withKeys = (list: AiGatewayKey[]) =>
  server.use(http.get("/api/v1/ai/keys", () => HttpResponse.json(list)));
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
const withCost = (total_usd: number, tokens_in: number) =>
  server.use(http.get("/api/v1/cost/summary", () => HttpResponse.json({
    window: "today", total_usd, tokens_in, tokens_out: 0, top_consumers: [], pct_of_budget: null,
  })));
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

  it("shows the gateway's two base URLs as copyable chips", async () => {
    renderApp(<GatewayOverview />);
    expect(await screen.findByRole("button", { name: "Copy URL https://tunnels.example.com/openai/v1" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy URL https://tunnels.example.com/anthropic" })).toBeInTheDocument();
  });

  it("has no checklist once there is a provider, a model, a key and a served request", async () => {
    const { qc } = renderApp(<GatewayOverview />);
    await screen.findByRole("table", { name: "Providers" });
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
  });

  it("with no provider at all: zero requests, and the checklist's first step instead of the list", async () => {
    withCost(0, 0);
    withProviders([]);
    withModels([]);
    withKeys([]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Add a provider", "to do", "true"],
      ["Create a model", "to do", "false"],
      ["Create a gateway key", "to do", "false"],
      ["Send the first request", "to do", "false"],
    ]);
    expect(within(list).getByRole("link", { name: "Add a provider" })).toHaveAttribute("href", "/gateway/providers");
    expect(tiles(await strip())[0]).toEqual(["Requests 24h", "0", "/gateway/requests"]);
    // Nothing to configure yet: the last step says what comes first instead of showing a snippet.
    const { default: userEvent } = await import("@testing-library/user-event");
    await userEvent.click(within(list).getByRole("button", { name: "Send the first request" }));
    const last = within(list).getAllByRole("listitem")[3]!;
    expect(within(last).getByText(/create a model first/i)).toBeInTheDocument();
    expect(within(last).queryByRole("tab")).toBeNull();
    expect(screen.queryByRole("table", { name: "Providers" })).toBeNull();
  });

  it("a provider but no model: the model step is open and leads to the Models page", async () => {
    withCost(0, 0);
    withProviders([provider({ requests_24h: 0 })]);
    withModels([]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Add a provider", "done", "false"],
      ["Create a model", "to do", "true"],
      ["Create a gateway key", "done", "false"],
      ["Send the first request", "to do", "false"],
    ]);
    expect(within(list).getByRole("link", { name: "Create a model" })).toHaveAttribute("href", "/gateway/models");
  });

  it("a model but no active key: the key step is open and leads to the Gateway keys page", async () => {
    withCost(0, 0);
    withProviders([provider({ requests_24h: 0 })]);
    // A revoked key is not a key anyone can use.
    withKeys([{ ...db.aiGatewayKeys[0]!, revoked_at: "2026-05-20T00:00:00Z" }]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list)).toEqual([
      ["Add a provider", "done", "false"],
      ["Create a model", "done", "false"],
      ["Create a gateway key", "to do", "true"],
      ["Send the first request", "to do", "false"],
    ]);
    expect(within(list).getByRole("link", { name: "Create a gateway key" })).toHaveAttribute("href", "/gateway/keys");
  });

  it("everything but a request: the last step shows the connect card with the gateway's base URL", async () => {
    withCost(0, 0);
    withProviders([provider({ requests_24h: 0 })]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list).map((s) => [s[1], s[2]])).toEqual([["done", "false"], ["done", "false"], ["done", "false"], ["to do", "true"]]);
    expect(within(list).getByRole("heading", { name: "Connect a client" })).toBeInTheDocument();
    expect(within(list).getByRole("button", { name: "Copy base URL https://tunnels.example.com/anthropic" })).toBeInTheDocument();
    // A placeholder stands where the key goes; the list never holds a key.
    expect(list.textContent).toContain("<your gateway key>");
    expect(list.textContent).not.toMatch(/bgw_/);
  });

  it("non-admin without an enabled model: the last step says whom to ask, not to create one", async () => {
    db.me = { ...db.me, role: "user" };
    withCost(0, 0);
    withProviders([provider({ requests_24h: 0 })]);
    withModels([]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    // The seeded key is theirs, so the last step is the open one.
    expect(stepStates(list)).toEqual([["Create a gateway key", "done", "false"], ["Send the first request", "to do", "true"]]);
    expect(within(list).getByText(/ask an administrator to create a model/i)).toBeInTheDocument();
    expect(within(list).queryByText(/create a model first/i)).toBeNull();
  });

  it.each([
    ["models", "Couldn't load models: boom"],
    ["keys", "Couldn't load gateway keys: boom"],
  ])("%s failing: an error with a retry instead of a checklist that says none exist", async (what, message) => {
    withCost(0, 0);
    withProviders([provider({ requests_24h: 0 })]);
    server.use(http.get(`/api/v1/ai/${what}`, () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    const { qc } = renderApp(<GatewayOverview />);
    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
    // The rest of the page is still there.
    expect(screen.getByRole("table", { name: "Providers" })).toBeInTheDocument();
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

  it("non-admin: no admin-only request and no error notice; setup starts at the gateway key", async () => {
    db.me = { ...db.me, role: "user" };
    const asked: string[] = [];
    const spy = (path: string) => http.get(`/api/v1/${path}`, () => { asked.push(path); return HttpResponse.json({ error: "admin required" }, { status: 403 }); });
    server.use(spy("settings"), spy("budgets"), spy("clients"));
    withCost(0, 0);
    withProviders([provider({ requests_24h: 0 })]);
    const { qc } = renderApp(<GatewayOverview />);
    const list = await checklist();
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(stepStates(list).map((s) => s[0])).toEqual(["Create a gateway key", "Send the first request"]);
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

  it("a quiet day does not reopen 'Send the first request': tokens counted today are proof too", async () => {
    withCost(0, 1200);
    withProviders([provider({ api_key_count: 1, requests_24h: 0 })]);
    const { qc } = renderApp(<GatewayOverview />);
    await screen.findByRole("table", { name: "Providers" });
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
  });

  it("so is a cost above zero", async () => {
    withCost(0.4, 0);
    withProviders([provider({ api_key_count: 1, requests_24h: 0 })]);
    const first = renderApp(<GatewayOverview />);
    await screen.findByRole("table", { name: "Providers" });
    await waitFor(() => expect(first.qc.isFetching()).toBe(0));
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
  });

  it("without any such proof the step stays open, and waits for the cost answer before saying so", async () => {
    withCost(0, 0);
    withProviders([provider({ api_key_count: 1, requests_24h: 0 })]);
    renderApp(<GatewayOverview />);
    const list = await checklist();
    expect(stepStates(list).map((s) => s[1])).toEqual(["done", "done", "done", "to do"]);
  });

  it("holds the checklist back while the cost answer is still out, then shows it", async () => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    server.use(http.get("/api/v1/cost/summary", async () => {
      await held;
      return HttpResponse.json({ window: "today", total_usd: 0, tokens_in: 0, tokens_out: 0, top_consumers: [], pct_of_budget: null });
    }));
    withProviders([provider({ api_key_count: 1, requests_24h: 0 })]);
    renderApp(<GatewayOverview />);
    // Everything else is there; only the list that depends on the cost is not.
    await screen.findByRole("table", { name: "Providers" });
    expect(screen.queryByRole("list", { name: "Set up the AI Gateway" })).toBeNull();
    release();
    expect(stepStates(await checklist()).map((s) => s[1])).toEqual(["done", "done", "done", "to do"]);
  });
});
