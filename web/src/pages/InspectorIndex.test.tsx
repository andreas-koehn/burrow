import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import { Routes, Route } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import InspectorIndex from "@/pages/InspectorIndex";

function mount() {
  return renderApp(
    <Routes>
      <Route path="/inspector" element={<InspectorIndex />} />
      <Route path="/inspector/:serviceId" element={<p>inspector for service</p>} />
    </Routes>,
    "/inspector",
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
    expect(screen.getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect");
  });
});
