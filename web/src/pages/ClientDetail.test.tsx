import { describe, it, expect } from "vitest";
import { http, HttpResponse } from "msw";
import { screen, within } from "@testing-library/react";
import { Routes, Route } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import ClientDetail from "@/pages/ClientDetail";

function mount() {
  return renderApp(
    <Routes><Route path="/clients/:id" element={<ClientDetail />} /></Routes>,
    "/clients/sess_4f7a9c0b2e81",
  );
}

// The default mock client only serves a tcp tunnel; the access tests need an
// http service backed by a durable service row next to it.
function useMixedClient() {
  server.use(
    http.get("/api/v1/clients/:id", () =>
      HttpResponse.json({
        session_id: "sess_4f7a9c0b2e81", user_id: "u1", token_name: "office-box-1",
        remote_addr: "203.0.113.7:51234", os: "linux", arch: "amd64", client_version: "0.2.0",
        service_count: 2, total_bytes_in: 0, total_bytes_out: 0,
        services: [
          { id: "tnl_http", name: "ollama", type: "http", remote_port: 0, local_addr: "127.0.0.1:11434", access_mode: "api_key", service_id: "svc-1", bytes_in: 0, bytes_out: 0, total_bytes_in: 0, total_bytes_out: 0 },
          { id: "tnl_tcp", name: "postgres", type: "tcp", remote_port: 9000, local_addr: "127.0.0.1:5432", access_mode: "open", bytes_in: 0, bytes_out: 0, total_bytes_in: 0, total_bytes_out: 0 },
        ],
      }),
    ),
  );
}

describe("Client detail", () => {
  it("shows client metadata and its services", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: /office-box-1/i })).toBeInTheDocument();
    const svcTable = screen.getByRole("table", { name: /services/i });
    expect(within(svcTable).getByText("web-staging")).toBeInTheDocument();
    expect(within(svcTable).getByText(":9000")).toBeInTheDocument();
  });

  it("shows 'client not found' for an unknown id", async () => {
    renderApp(
      <Routes><Route path="/clients/:id" element={<ClientDetail />} /></Routes>,
      "/clients/nope",
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(/not found/i);
  });

  it("shows the access mode as a badge and links http services to their service page", async () => {
    useMixedClient();
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByText("API key")).toBeInTheDocument();
    expect(within(table).getByText("Open")).toBeInTheDocument();
    // Only the http service has a durable service row, so exactly one link.
    expect(within(table).getByRole("link", { name: /configure/i }))
      .toHaveAttribute("href", "/services/svc-1");
  });

  it("does not embed the access-mode editor in the table", async () => {
    useMixedClient();
    mount();
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByText("ollama")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Access mode" })).toBeNull();
    expect(screen.queryByRole("button", { name: /save changes/i })).toBeNull();
  });
});
