import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import RequestInspector from "@/pages/RequestInspector";

function mount(path = "/gateway/requests/svc_ai001") {
  return renderApp(
    <Routes>
      <Route path="/gateway/requests/:serviceId/:requestId?" element={<RequestInspector />} />
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
        "Request inspector is off for this service — enable in Access settings.",
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

  it("lets the user switch to another http service (F7)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "svc-a", name: "alpha", type: "http", access_mode: "api_key", connected: true },
      { id: "svc-b", name: "beta", type: "http", access_mode: "api_key", connected: false },
    ])));
    mount();
    const picker = await screen.findByRole("button", { name: /service/i });
    expect(picker).toHaveAttribute("id", "inspector-service");
    await userEvent.click(picker);
    await userEvent.click(await screen.findByRole("option", { name: "beta" }));
    // Picking a service navigates to its inspector: the trigger now shows it.
    await waitFor(() => {
      expect(document.getElementById("inspector-service")).toHaveTextContent("beta");
    });
  });

  it("shows no service picker when there is only one http service (F7)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "svc_ai001", name: "ollama", type: "http", access_mode: "api_key", connected: true },
      { id: "tcp-1", name: "ssh", type: "tcp", access_mode: "open", connected: true },
    ])));
    mount();
    // The subtitle names the service only once /services has resolved.
    await screen.findByText("Tail and replay traffic on ollama.");
    expect(document.getElementById("inspector-service")).toBeNull();
  });

  it("shows the API error with a way back for an unknown service, not an endless skeleton", async () => {
    mount("/gateway/requests/no-such-service");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load requests: service not found");
    expect(within(alert).getByRole("link", { name: /request inspector/i })).toHaveAttribute("href", "/gateway/requests");
    expect(screen.queryByRole("table", { name: /requests/i })).not.toBeInTheDocument();
  });

  it("clears the search filter when switching service (F7)", async () => {
    db.services.push({ ...db.services.find((s) => s.id === "svc_ai001")!, id: "svc_other", name: "other" });
    db.inspectorEntries.svc_other = db.inspectorEntries.svc_ai001!;
    mount();
    const search = await screen.findByRole("searchbox", { name: /search requests/i });
    await userEvent.type(search, "zzz-no-match");
    expect(await screen.findByText("No requests match your search.")).toBeInTheDocument();
    expect(screen.queryByText("No requests yet.")).not.toBeInTheDocument();
    expect(screen.queryByText(/Requests appear here as soon as traffic/)).not.toBeInTheDocument();
    await userEvent.click(document.getElementById("inspector-service")!);
    await userEvent.click(await screen.findByRole("option", { name: "other" }));
    await waitFor(() => {
      expect(screen.getByRole("searchbox", { name: /search requests/i })).toHaveValue("");
    });
    expect(screen.queryByText("No requests yet.")).not.toBeInTheDocument();
    expect(screen.queryByText("No requests match your search.")).not.toBeInTheDocument();
  });

  it("keeps the loaded list when a background refetch fails", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const rows = await within(table).findAllByRole("row");
    await userEvent.click(rows[1]!);
    // From here on the list endpoint fails; a replay invalidates and refetches it.
    let failed = 0;
    server.use(http.get("/api/v1/services/:id/inspector/requests", () => {
      failed += 1;
      return HttpResponse.json({ error: "boom" }, { status: 500 });
    }));
    await userEvent.click(await screen.findByRole("button", { name: /open replay dialog/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^replay$/i }));
    await waitFor(() => expect(failed).toBeGreaterThan(0));
    // Let the failed refetch settle into the query state before asserting.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByRole("table", { name: /requests/i })).toBeInTheDocument();
    expect(screen.queryByText(/couldn't load requests/i)).not.toBeInTheDocument();
  });

  it("the unknown-service error offers a Retry that refetches the list", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/services/:id/inspector/requests", () => {
      calls += 1;
      if (calls === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json([]);
    }));
    mount();
    const alert = await screen.findByRole("alert");
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: /requests/i })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
