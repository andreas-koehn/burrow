import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import RequestInspector from "@/pages/RequestInspector";

function mount(path = "/inspector/svc_ai001") {
  return renderApp(
    <Routes>
      <Route path="/inspector/:serviceId/:requestId?" element={<RequestInspector />} />
    </Routes>,
    path,
  );
}

describe("Request inspector (§4.23)", () => {
  it("renders a two-pane layout with the request list on the left", async () => {
    mount();
    expect(await screen.findByRole("table", { name: /requests/i })).toBeInTheDocument();
    // The right pane prompts the user to pick a request when none selected.
    expect(screen.getByText(/select a request/i)).toBeInTheDocument();
  });

  it("clicking a row populates the right pane with the redacted headers table", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const rows = await within(table).findAllByRole("row");
    // skip the header row
    await userEvent.click(rows[1]!);
    expect(await screen.findByRole("tab", { name: /^request$/i })).toBeInTheDocument();
    // The headers table appears with at least one redacted value cell.
    const headersTable = await screen.findByRole("table", { name: /^headers$/i });
    expect(within(headersTable).getByText(/\[redacted\]/)).toBeInTheDocument();
  });

  it("shows the off-message verbatim when inspector.enabled is false", async () => {
    db.aiConfigs.svc_ai001.inspector.enabled = false;
    mount();
    expect(
      await screen.findByText(
        "Request inspector is off for this tunnel — enable in Access settings.",
      ),
    ).toBeInTheDocument();
  });

  it("Replay POSTs /services/:id/inspector/requests/:rid/replay", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const rows = await within(table).findAllByRole("row");
    await userEvent.click(rows[1]!);
    // Open the replay dialog from the detail toolbar, then confirm.
    await userEvent.click(await screen.findByRole("button", { name: /open replay dialog/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^replay$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          /\/api\/v1\/services\/svc_ai001\/inspector\/requests\/[^/]+\/replay$/.test(String(url))
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
  });

  it("inspector rows are reachable via table[aria-label=Requests] tbody tr.clickable (capture-harness guard)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const clickable = within(table).getAllByRole("row").filter((r) => r.classList.contains("clickable"));
    expect(clickable.length).toBeGreaterThan(0);
  });
});
