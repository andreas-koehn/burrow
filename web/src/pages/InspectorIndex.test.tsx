import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Routes, Route } from "react-router-dom";
import { delay, http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import InspectorIndex from "@/pages/InspectorIndex";

function mount() {
  return renderApp(
    <Routes>
      <Route path="/gateway/requests" element={<InspectorIndex />} />
      <Route path="/gateway/requests/:serviceId" element={<p>inspector for service</p>} />
      <Route path="/clients/connect" element={<p>connect a client page</p>} />
    </Routes>,
    "/gateway/requests",
  );
}

describe("InspectorIndex (F7)", () => {
  it("redirects to the first http service", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "tcp-1", name: "ssh", type: "tcp", access_mode: "open", connected: true },
      { id: "http-1", name: "ollama", type: "http", access_mode: "api_key", connected: true },
    ])));
    mount();
    expect(await screen.findByText("inspector for service")).toBeInTheDocument();
  });

  it("shows an empty state when there is no http service", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "tcp-1", name: "ssh", type: "tcp", access_mode: "open", connected: true },
    ])));
    mount();
    expect(await screen.findByText("No HTTP services to inspect")).toBeInTheDocument();
    // A plain button, not a button nested in a link.
    expect(screen.queryByRole("link", { name: "Connect a client" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Connect a client" }));
    expect(await screen.findByText("connect a client page")).toBeInTheDocument();
  });

  it("shows an error, not the empty state, when the service list fails; Retry recovers", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/services", () => {
      calls += 1;
      if (calls === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json([
        { id: "http-1", name: "ollama", type: "http", access_mode: "api_key", connected: true },
      ]);
    }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load services");
    expect(screen.queryByText("No HTTP services to inspect")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Connect a client" })).not.toBeInTheDocument();
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("inspector for service")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows a skeleton while the service list is loading", async () => {
    server.use(http.get("/api/v1/services", async () => {
      await delay(150);
      return HttpResponse.json([]);
    }));
    const { container } = mount();
    expect(container.querySelector(".skel")).not.toBeNull();
    expect(screen.queryByText("No HTTP services to inspect")).not.toBeInTheDocument();
    expect(await screen.findByText("No HTTP services to inspect")).toBeInTheDocument();
    expect(container.querySelector(".skel")).toBeNull();
  });
});
