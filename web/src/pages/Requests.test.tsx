import { describe, it, expect, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useLocation } from "react-router-dom";
import { delay, http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import type { InspectorEntry } from "@/lib/contract";
import Requests from "@/pages/Requests";

function Probe() {
  const { pathname, search } = useLocation();
  return <div data-testid="path">{pathname + search}</div>;
}

function mount(route = "/gateway/requests") {
  return renderApp(<><Requests /><Probe /></>, route);
}

const minutesAgo = (n: number) => new Date(Date.now() - n * 60_000).toISOString();

/** Captured requests of a service, the first one `ages[0]` minutes old. */
function capture(serviceId: string, ages: number[], over: Partial<InspectorEntry> = {}) {
  const seed = db.inspectorEntries.svc_ai001![0]!;
  db.inspectorEntries[serviceId] = ages.map((age, i) => ({
    ...seed, id: `ie_${serviceId}_${i}`, service_id: serviceId, ts: minutesAgo(age), ...over,
  }));
}

const table = () => screen.findByRole("table", { name: "Requests" });
const bodyRows = (t: HTMLElement) => Array.from(t.querySelectorAll<HTMLElement>("tbody tr"));

describe("Requests page", () => {
  afterEach(() => resetDb());

  it("renders the heading 'Requests'", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: /^requests$/i, level: 1 })).toBeInTheDocument();
  });

  it("names the table Requests and shows the columns the relay records", async () => {
    capture("svc_web01", [1, 2]);
    mount();
    const t = await table();
    // Provider, model, tokens and cost are not part of a captured request yet.
    expect(within(t).getAllByRole("columnheader").map((h) => h.textContent))
      .toEqual(["Time", "Method", "Path", "Status", "Cache", "Guardrail"]);
  });

  it("starts on the first http service", async () => {
    // svc_web01 is the first http service of the seed; svc_ai001 has requests of its own.
    capture("svc_web01", [1, 2, 3]);
    capture("svc_ai001", [1]);
    mount();
    expect(bodyRows(await table())).toHaveLength(3);
    expect(screen.getByRole("combobox", { name: "Service" })).toHaveValue("svc_web01");
  });

  it("a row links to /gateway/requests/<serviceId>/<requestId>", async () => {
    capture("svc_web01", [1]);
    mount();
    const t = await table();
    expect(within(t).getByRole("link", { name: "/v1/chat/completions" }))
      .toHaveAttribute("href", "/gateway/requests/svc_web01/ie_svc_web01_0");
  });

  it("the service picker offers the http services only, switches the list and writes ?service=<id>", async () => {
    capture("svc_web01", [1]);
    capture("svc_ai001", [1, 2]);
    mount();
    await table();
    const picker = screen.getByRole("combobox", { name: "Service" });
    expect(within(picker).getAllByRole("option").map((o) => o.textContent)).toEqual(["web", "ollama", "grafana"]);
    await userEvent.selectOptions(picker, "svc_ai001");
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?service=svc_ai001");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(2));
  });

  it("opening /gateway/requests?service=svc_ai001 starts on that service", async () => {
    capture("svc_web01", [1]);
    capture("svc_ai001", [1, 2]);
    mount("/gateway/requests?service=svc_ai001");
    expect(bodyRows(await table())).toHaveLength(2);
  });

  it("shows status, cache and the number of redactions", async () => {
    capture("svc_web01", [1], { status: 502, cache: "HIT", redactions: [{ rule: "email", count: 2 }, { rule: "iban", count: 1 }] });
    mount();
    const row = within(bodyRows(await table())[0]!);
    expect(row.getByText("502")).toHaveClass("badge", "status-5xx");
    expect(row.getByText("HIT")).toBeInTheDocument();
    expect(row.getByText("3 redacted")).toBeInTheDocument();
  });

  it("the time range narrows the list and is kept in the URL", async () => {
    capture("svc_web01", [5, 30, 600]);
    mount();
    expect(bodyRows(await table())).toHaveLength(3);
    await userEvent.click(screen.getByRole("radio", { name: "15 min" }));
    expect(bodyRows(await table())).toHaveLength(1);
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?range=15m");
  });

  it("the Filter box searches path, method and body", async () => {
    capture("svc_web01", [1, 2]);
    db.inspectorEntries.svc_web01![1] = { ...db.inspectorEntries.svc_web01![1]!, path: "/v1/embeddings" };
    mount();
    await table();
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "embed");
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(1));
    expect(screen.getByTestId("path")).toHaveTextContent("/gateway/requests?q=embed");
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "zzz");
    expect(await screen.findByText("No requests match your filter")).toBeInTheDocument();
  });

  it("requests older than the period: says so and offers the last 7 days", async () => {
    capture("svc_web01", [3 * 24 * 60]);
    mount();
    expect(await screen.findByText("No requests in this period")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show the last 7 days" }));
    expect(bodyRows(await table())).toHaveLength(1);
  });

  it("a service without any captured request says how the list fills", async () => {
    mount();
    expect(await screen.findByText("No requests yet")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show the last 7 days" })).toBeNull();
  });

  it("says when capturing is switched off for the service", async () => {
    db.aiConfigs.svc_web01 = { inspector: { enabled: false, max_requests: 100 } } as never;
    mount();
    expect(await screen.findByText("Request capture is off for web")).toBeInTheDocument();
    expect(screen.getByText("Requests to this service are not being recorded.")).toBeInTheDocument();
  });

  it("with no provider at all: 'No providers yet' and a link to add one", async () => {
    db.services = db.services.filter((s) => s.type !== "http");
    mount();
    expect(await screen.findByText("No providers yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add a provider" })).toHaveAttribute("href", "/gateway/providers");
    // The page itself stays: heading and all.
    expect(screen.getByRole("heading", { name: /^requests$/i, level: 1 })).toBeInTheDocument();
  });

  it("shows an error, not the empty state, when the service list fails; Retry recovers", async () => {
    capture("svc_web01", [1]);
    let calls = 0;
    server.use(http.get("/api/v1/services", () => {
      calls += 1;
      if (calls === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json(db.services);
    }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load requests: boom");
    expect(screen.queryByText("No providers yet")).not.toBeInTheDocument();
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(bodyRows(await table())).toHaveLength(1);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows an error when the requests of the service cannot be loaded", async () => {
    server.use(http.get("/api/v1/services/:id/inspector/requests", () => HttpResponse.json({ error: "nope" }, { status: 500 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load requests: nope");
  });

  it("shows a skeleton while the service list is loading", async () => {
    server.use(http.get("/api/v1/services", async () => {
      await delay(150);
      return HttpResponse.json([]);
    }));
    const { container } = mount();
    expect(container.querySelector(".skel")).not.toBeNull();
    expect(screen.queryByText("No providers yet")).not.toBeInTheDocument();
    expect(await screen.findByText("No providers yet")).toBeInTheDocument();
    expect(container.querySelector(".skel")).toBeNull();
  });
});
