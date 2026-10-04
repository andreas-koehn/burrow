import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import ConnectionLogs from "@/pages/ConnectionLogs";

function mount() {
  return renderApp(<ConnectionLogs />, "/connection-logs");
}

describe("Connection logs page (§v0.5.0 Part E)", () => {
  it("renders an unknown status without crashing the page", async () => {
    db.connectionLogs[0] = { ...db.connectionLogs[0], status: "half_open" as never };
    mount();
    const table = await screen.findByRole("table", { name: /connection logs/i });
    await waitFor(() => {
      const badge = table.querySelector("td[data-status='half_open'] span.badge");
      expect(badge?.textContent).toBe("half open");
    });
  });

  afterEach(() => {
    resetDb();
  });

  it("renders the heading 'Connection logs'", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: /^connection logs$/i })).toBeInTheDocument();
  });

  it("renders the seeded rows in the table", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /connection logs/i });
    const rows = Array.from(table.querySelectorAll("tbody tr"));
    expect(rows.length).toBeGreaterThan(5);
    // mono small cells (source IP or started_at) should be visible
    const monoCells = Array.from(table.querySelectorAll("td.mono"));
    expect(monoCells.length).toBeGreaterThan(0);
  });

  it("empty state shows verbatim text when zero rows", async () => {
    db.connectionLogs = [];
    mount();
    // EmptyState renders <h4>title</h4> + <p>body</p> separately.
    expect(await screen.findByText("No connection logs yet")).toBeInTheDocument();
    expect(screen.getByText(/connections are recorded on session close/i)).toBeInTheDocument();
  });

  it("Rollups toggle switches the table to the rollups endpoint", async () => {
    mount();
    // Wait for initial table to load
    await screen.findByRole("table", { name: /connection logs/i });
    // Click the rollups toggle (native checkbox rendered with aria-label="Rollups")
    const toggle = await screen.findByRole("checkbox", { name: /rollups/i });
    await userEvent.click(toggle);
    // Rollups table should show "Day" column header
    await waitFor(() => {
      expect(screen.getByText("Day")).toBeInTheDocument();
    });
    // At least one rollup row should be visible
    const table = screen.getByRole("table", { name: /connection logs/i });
    const rows = Array.from(table.querySelectorAll("tbody tr"));
    expect(rows.length).toBeGreaterThan(0);
  });

  it("Rollups view renders Top source IPs column when API returns the field", async () => {
    // v0.5.1 Q12: the seeded MSW response has 3 rollup rows; the first two
    // carry top_source_ips, the third omits it (operator toggle OFF). The
    // component renders the column header whenever ANY row in the page
    // includes the field, and each cell shows "ip (sessions), …" or "—" /
    // empty depending on the per-row state.
    mount();
    await screen.findByRole("table", { name: /connection logs/i });
    const toggle = await screen.findByRole("checkbox", { name: /rollups/i });
    await userEvent.click(toggle);

    // Column header appears.
    await waitFor(() => {
      expect(screen.getByText("Top source IPs")).toBeInTheDocument();
    });

    // The first seeded row carries [{10.0.0.1: 42}, {10.0.0.2: 17}].
    const cells = screen.getAllByTestId("top-source-ips");
    expect(cells.length).toBeGreaterThan(0);
    // At least one cell should contain a "ip (sessions)" rendering.
    expect(
      cells.some((c) => /10\.0\.0\.\d+ \(\d+\)/.test(c.textContent ?? "")),
    ).toBe(true);
  });

  it("Rollups view renders '—' for both undefined and empty top_source_ips (BACKLOG_0.5.2 #6)", async () => {
    // v0.5.2 BACKLOG #6: pre-fix the cell rendered "" for undefined and "—"
    // for empty []. Post-fix the empty-state rendering is uniform: any group
    // with no source-IPs (undefined OR empty array) shows "—".
    //
    // Seed is 3 rows at dayOffset(2)/dayOffset(1)/dayOffset(0); the default
    // 24h date filter shows only the last two (indexes 1 and 2). To exercise
    // both empty-state branches we put `undefined` on index 1 and `[]` on
    // index 2, with index 0 untouched (it's filtered out anyway).
    db.connectionLogRollups = [
      { ...db.connectionLogRollups[0]! }, // filtered out by 24h preset
      { ...db.connectionLogRollups[1]!, top_source_ips: undefined },
      { ...db.connectionLogRollups[2]!, top_source_ips: [] },
    ];
    mount();
    await screen.findByRole("table", { name: /connection logs/i });
    const toggle = await screen.findByRole("checkbox", { name: /rollups/i });
    await userEvent.click(toggle);
    await waitFor(() => {
      expect(screen.getByText("Top source IPs")).toBeInTheDocument();
    });

    const cells = screen.getAllByTestId("top-source-ips");
    // We expect 2 visible cells (one per row in the 24h window) and BOTH
    // must render "—" — the undefined branch (pre-fix rendered "") AND the
    // empty-array branch (already rendered "—" pre-fix).
    expect(cells.length).toBeGreaterThanOrEqual(2);
    const texts = cells.map((c) => c.textContent ?? "");
    const dashCount = texts.filter((t) => t === "—").length;
    expect(dashCount).toBeGreaterThanOrEqual(2);
    // Crucially: no cell renders as "" (blank) — that would be the pre-fix
    // asymmetric branch on undefined.
    expect(texts.some((t) => t === "")).toBe(false);
  });

  it("Rollups view omits Top source IPs header when ALL rows lack the field", async () => {
    // Strip top_source_ips from every seeded row to simulate the operator
    // having connection_logs.rollup_include_top_ips=false globally.
    db.connectionLogRollups = db.connectionLogRollups.map((r) => ({
      ...r,
      top_source_ips: undefined,
    }));
    mount();
    await screen.findByRole("table", { name: /connection logs/i });
    const toggle = await screen.findByRole("checkbox", { name: /rollups/i });
    await userEvent.click(toggle);
    await waitFor(() => {
      expect(screen.getByText("Day")).toBeInTheDocument();
    });
    // The Top source IPs header must NOT appear when no row carries the field.
    expect(screen.queryByText("Top source IPs")).toBeNull();
  });

  it("Export button triggers GET /connection-logs/export with format=ndjson", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /^export$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url]) =>
          String(url).includes("/api/v1/connection-logs/export") &&
          String(url).includes("format=ndjson"),
        ),
      ).toBe(true);
    });
  });

  it("Kind filter narrows to the chosen kind", async () => {
    mount();
    // Wait for initial table
    await screen.findByRole("table", { name: /connection logs/i });
    // Select "control" from the native kind select
    const kindSelect = await screen.findByRole("combobox", { name: /kind/i });
    await userEvent.selectOptions(kindSelect, "control");
    // Table still renders rows (at least some control rows seeded)
    await waitFor(() => {
      const table = screen.getByRole("table", { name: /connection logs/i });
      const rows = Array.from(table.querySelectorAll("tbody tr"));
      expect(rows.length).toBeGreaterThan(0);
    });
    // Verify only control rows are shown (no http_proxy or tcp_proxy badges)
    const kindCells = Array.from(
      document.querySelectorAll("td[data-kind]"),
    );
    if (kindCells.length > 0) {
      kindCells.forEach((cell) => {
        expect(cell.getAttribute("data-kind")).toBe("control");
      });
    }
  });

  it("renders service name (not UUID) when a service is known (B4)", async () => {
    // The MSW mock serves db.services for /api/v1/services and db.connectionLogs
    // for /api/v1/connection-logs. The seeded connectionLogs use service IDs like
    // "svc_web01" whose name in db.services is "web". After the fix, the SERVICE
    // column must show "web" not "svc_web01".
    mount();
    // Wait for the table to render (logs view, not rollups)
    await screen.findByRole("table", { name: /connection logs/i });
    await waitFor(() => {
      // "web" is the name for svc_web01 — multiple seeded rows use this service.
      expect(screen.getAllByText("web").length).toBeGreaterThan(0);
      // The raw UUID "svc_web01" must NOT appear anywhere in the document.
      expect(screen.queryByText("svc_web01")).toBeNull();
    });
  });

  it("renders STATUS as a colored badge and right-aligns numeric columns (D-10/L-8/L-11)", async () => {
    // The seed contains rows with status "closed_clean" (first seeded row).
    // closed_clean → status-connected (green), rejected → status-suspended (red),
    // closed_error → status-suspended, closed_idle → status-idle (amber).
    mount();
    await screen.findByRole("table", { name: /connection logs/i });

    // (a) A STATUS cell should render a <span class="badge status-connected">
    //     for a closed_clean row. Find the badge inside the status cell.
    await waitFor(() => {
      // The seed's first row has status "closed_clean"; findAllByText finds all badge spans.
      const badges = document.querySelectorAll("td[data-status='closed_clean'] span.badge");
      expect(badges.length).toBeGreaterThan(0);
      expect(badges[0]).toHaveClass("status-connected");
      expect(badges[0].textContent).toBe("closed clean");
    });

    // Also verify a rejected row maps to status-suspended.
    const rejectedBadges = document.querySelectorAll("td[data-status='rejected'] span.badge");
    expect(rejectedBadges.length).toBeGreaterThan(0);
    expect(rejectedBadges[0]).toHaveClass("status-suspended");

    // (b) The "Duration" column header must carry the col-num class.
    const durHeader = screen.getByRole("columnheader", { name: /^duration$/i });
    expect(durHeader).toHaveClass("col-num");

    // Bytes in and Bytes out headers also get col-num.
    const bytesInHeader = screen.getByRole("columnheader", { name: /^bytes in$/i });
    expect(bytesInHeader).toHaveClass("col-num");
    const bytesOutHeader = screen.getByRole("columnheader", { name: /^bytes out$/i });
    expect(bytesOutHeader).toHaveClass("col-num");
  });
});
