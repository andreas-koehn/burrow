import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useLocation } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import Traffic from "@/pages/Traffic";

function Probe() {
  const { pathname, search } = useLocation();
  return <div data-testid="path">{pathname + search}</div>;
}

function mount(route = "/traffic") {
  return renderApp(<><Traffic /><Probe /></>, route);
}

const table = () => screen.findByRole("table", { name: "Traffic" });
const bodyRows = (t: HTMLElement) => Array.from(t.querySelectorAll<HTMLElement>("tbody tr"));
/** The Service cell of every row. */
const serviceCells = (t: HTMLElement) => bodyRows(t).map((tr) => tr.querySelectorAll("td")[1]?.textContent);

describe("Traffic page", () => {
  afterEach(() => {
    resetDb();
    vi.restoreAllMocks();
  });

  it("renders the heading 'Traffic' and no way back to Settings", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: /^traffic$/i, level: 1 })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /settings/i })).toBeNull();
  });

  it("names the table Traffic and shows the default columns", async () => {
    mount();
    const t = await table();
    expect(within(t).getAllByRole("columnheader").map((h) => h.textContent))
      .toEqual(["Time", "Service", "Client IP", "Protocol", "Status", "Duration", "Bytes"]);
  });

  it("renders the seeded rows in the table", async () => {
    mount();
    const t = await table();
    expect(bodyRows(t).length).toBeGreaterThan(5);
    // Source IP and timestamps are set in the mono face.
    expect(t.querySelectorAll("td .mono").length).toBeGreaterThan(0);
  });

  it("renders an unknown status without crashing the page", async () => {
    db.connectionLogs[0] = { ...db.connectionLogs[0], status: "half_open" as never };
    mount();
    const t = await table();
    await waitFor(() => {
      expect(t.querySelector("[data-status='half_open'] .badge")?.textContent).toBe("half open");
    });
  });

  it("the empty state reads 'No traffic in this period' and offers the last 7 days", async () => {
    // One connection three days ago: outside the default 24 hours, inside 7 days.
    db.connectionLogs = [{ ...db.connectionLogs[0]!, started_at: new Date(Date.now() - 3 * 86_400_000).toISOString() }];
    mount();
    expect(await screen.findByText("No traffic in this period")).toBeInTheDocument();
    expect(screen.getByText(/connections are recorded on session close/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show the last 7 days" }));
    expect(bodyRows(await table())).toHaveLength(1);
    expect(screen.getByRole("radio", { name: "7 days" })).toBeChecked();
    expect(screen.getByTestId("path")).toHaveTextContent("/traffic?range=7d");
  });

  it("does not offer a longer period when 7 days are already shown", async () => {
    db.connectionLogs = [];
    mount("/traffic?range=7d");
    expect(await screen.findByText("No traffic in this period")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show the last 7 days" })).toBeNull();
  });

  it("the time range is sent to the API and kept in the URL", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await table();
    await userEvent.click(screen.getByRole("radio", { name: "1 hour" }));
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/traffic?range=1h"));
    await waitFor(() => {
      const since = fetchSpy.mock.calls
        .map(([url]) => new URL(String(url), "http://x").searchParams.get("since"))
        .filter((s): s is string => Boolean(s))
        .at(-1)!;
      const age = Date.now() - Date.parse(since);
      expect(age).toBeGreaterThan(3_500_000);
      expect(age).toBeLessThan(3_700_000);
    });
  });

  it("the Filter box searches on the server and is kept in the URL", async () => {
    mount();
    await table();
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "rejected");
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/traffic?q=rejected"));
    await waitFor(async () => {
      const rows = bodyRows(await table());
      expect(rows.length).toBeGreaterThan(0);
      for (const tr of rows) expect(tr.querySelector("[data-status]")).toHaveAttribute("data-status", "rejected");
    });
  });

  it("the service picker narrows the list and writes ?service=<id>", async () => {
    mount();
    await table();
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Service" }), "svc_web01");
    expect(screen.getByTestId("path")).toHaveTextContent("/traffic?service=svc_web01");
    await waitFor(async () => {
      const cells = serviceCells(await table());
      expect(cells.length).toBeGreaterThan(0);
      expect(new Set(cells)).toEqual(new Set(["web"]));
    });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Service" }), "");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/traffic$/);
  });

  it("opening /traffic?service=svc_web01 starts narrowed", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount("/traffic?service=svc_web01");
    const cells = serviceCells(await table());
    expect(new Set(cells)).toEqual(new Set(["web"]));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Service" })).toHaveValue("svc_web01"));
    // Never asked for the unfiltered list first.
    const listCalls = fetchSpy.mock.calls.map(([url]) => String(url)).filter((u) => u.includes("/api/v1/connection-logs?"));
    expect(listCalls.length).toBeGreaterThan(0);
    for (const u of listCalls) expect(u).toContain("service_id=svc_web01");
  });

  it("Protocol filter narrows to the chosen kind", async () => {
    mount();
    await table();
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Protocol" }), "control");
    await waitFor(async () => {
      const cells = Array.from((await table()).querySelectorAll("[data-kind]"));
      expect(cells.length).toBeGreaterThan(0);
      for (const cell of cells) expect(cell).toHaveAttribute("data-kind", "control");
    });
  });

  it("renders service name (not UUID) when a service is known (B4)", async () => {
    mount();
    await table();
    await waitFor(() => {
      expect(screen.getAllByText("web", { selector: "td" }).length).toBeGreaterThan(0);
      expect(screen.queryByText("svc_web01", { selector: "td, td *" })).toBeNull();
    });
  });

  it("renders Status as a colored badge and right-aligns numeric columns (D-10/L-8/L-11)", async () => {
    mount();
    const t = await table();
    await waitFor(() => {
      const badges = t.querySelectorAll("[data-status='closed_clean'] .badge");
      expect(badges.length).toBeGreaterThan(0);
      expect(badges[0]).toHaveClass("status-connected");
      expect(badges[0].textContent).toBe("closed clean");
    });
    const rejected = t.querySelectorAll("[data-status='rejected'] .badge");
    expect(rejected.length).toBeGreaterThan(0);
    expect(rejected[0]).toHaveClass("status-suspended");

    expect(screen.getByRole("columnheader", { name: /^duration$/i })).toHaveClass("col-num");
    expect(screen.getByRole("columnheader", { name: /^bytes$/i })).toHaveClass("col-num");
    expect(screen.getByRole("columnheader", { name: /^status$/i })).not.toHaveClass("col-num");
  });

  it("shows bytes received and sent in one column", async () => {
    db.connectionLogs = [{ ...db.connectionLogs[0]!, bytes_in: 2048, bytes_out: 512 }];
    mount();
    const t = await table();
    expect(within(t).getByRole("cell", { name: "2.0K in / 512 out" })).toBeInTheDocument();
  });

  it("a row opens its detail: reason, user agent and the session it belonged to", async () => {
    db.connectionLogs = [{ ...db.connectionLogs[0]!, status: "rejected", reason: "ip not allowed" }];
    mount();
    const t = await table();
    await userEvent.click(bodyRows(t)[0]!);
    const panel = screen.getByRole("region", { name: "Traffic details" });
    expect(panel).toHaveTextContent("ip not allowed");
    expect(panel).toHaveTextContent("burrow-client/0.5.0");
    expect(panel).toHaveTextContent("sess_001");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("region", { name: "Traffic details" })).toBeNull();
  });

  it("shows an error, not the empty state, when the list fails; Retry recovers", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/connection-logs", () => {
      calls += 1;
      if (calls === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json([db.connectionLogs[0]]);
    }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load traffic");
    expect(screen.queryByText("No traffic in this period")).toBeNull();
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(bodyRows(await table())).toHaveLength(1);
  });

  it("Load more appends the next page and goes away on the last one", async () => {
    const one = db.connectionLogs[0]!;
    db.connectionLogs = Array.from({ length: 60 }, (_, i) => ({
      ...one,
      id: `cl_x${String(i).padStart(3, "0")}`,
      started_at: new Date(Date.now() - (i + 1) * 60_000).toISOString(),
    }));
    mount();
    expect(bodyRows(await table())).toHaveLength(50);
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(async () => expect(bodyRows(await table())).toHaveLength(60));
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("has no Load more when the first page is not full", async () => {
    mount();
    await table();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("Rollups toggle switches the table to the rollups endpoint", async () => {
    mount();
    await table();
    await userEvent.click(await screen.findByRole("checkbox", { name: /rollups/i }));
    expect(await screen.findByRole("columnheader", { name: "Day" })).toBeInTheDocument();
    expect(bodyRows(await table()).length).toBeGreaterThan(0);
  });

  it("Rollups view renders Top source IPs column when API returns the field", async () => {
    // v0.5.1 Q12: the column appears whenever any row in the page carries the field.
    mount();
    await table();
    await userEvent.click(await screen.findByRole("checkbox", { name: /rollups/i }));
    expect(await screen.findByRole("columnheader", { name: "Top source IPs" })).toBeInTheDocument();
    const cells = screen.getAllByTestId("top-source-ips");
    expect(cells.some((c) => /10\.0\.0\.\d+ \(\d+\)/.test(c.textContent ?? ""))).toBe(true);
  });

  it("Rollups view renders '—' for both undefined and empty top_source_ips (BACKLOG_0.5.2 #6)", async () => {
    // Seed is 3 rows two days ago, yesterday and today; the default 24 hours show the last two.
    db.connectionLogRollups = [
      { ...db.connectionLogRollups[0]! },
      { ...db.connectionLogRollups[1]!, top_source_ips: undefined },
      { ...db.connectionLogRollups[2]!, top_source_ips: [] },
    ];
    // The header only shows when a row carries the field: the empty array does.
    mount();
    await table();
    await userEvent.click(await screen.findByRole("checkbox", { name: /rollups/i }));
    expect(await screen.findByRole("columnheader", { name: "Top source IPs" })).toBeInTheDocument();
    const texts = screen.getAllByTestId("top-source-ips").map((c) => c.textContent ?? "");
    expect(texts.length).toBeGreaterThanOrEqual(2);
    expect(texts.filter((t) => t === "—").length).toBeGreaterThanOrEqual(2);
    expect(texts.some((t) => t === "")).toBe(false);
  });

  it("Rollups view omits Top source IPs header when ALL rows lack the field", async () => {
    db.connectionLogRollups = db.connectionLogRollups.map((r) => ({ ...r, top_source_ips: undefined }));
    mount();
    await table();
    await userEvent.click(await screen.findByRole("checkbox", { name: /rollups/i }));
    expect(await screen.findByRole("columnheader", { name: "Day" })).toBeInTheDocument();
    expect(screen.queryByText("Top source IPs")).toBeNull();
  });

  it("Rollups view has its own empty state", async () => {
    db.connectionLogRollups = [];
    mount();
    await table();
    await userEvent.click(await screen.findByRole("checkbox", { name: /rollups/i }));
    expect(await screen.findByText("No rollups in this period")).toBeInTheDocument();
  });

  it("Export button triggers GET /connection-logs/export with format=ndjson and the filters", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount("/traffic?service=svc_web01");
    await userEvent.click(await screen.findByRole("button", { name: /^export$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url]) =>
          String(url).includes("/api/v1/connection-logs/export") &&
          String(url).includes("format=ndjson") &&
          String(url).includes("service_id=svc_web01"),
        ),
      ).toBe(true);
    });
  });
});
