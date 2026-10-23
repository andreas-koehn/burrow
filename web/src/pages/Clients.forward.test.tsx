import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import Clients from "@/pages/Clients";
import ClientDetail from "@/pages/ClientDetail";

const since = () => new Date(Date.now() - 10 * 60_000).toISOString();

describe("forwarding sessions", () => {
  it("the client detail lists the forwards: target, since and bytes", async () => {
    db.clients[0].forward_count = 1;
    db.clients[0].forwards = [{ id: "c_f1", service_id: "svc_pg001", service: "postgres", since: since(), bytes_in: 2048, bytes_out: 4096 }];
    renderApp(<Routes><Route path="/clients/:id" element={<ClientDetail />} /></Routes>, `/clients/${db.clients[0].session_id}`);
    const table = await screen.findByRole("table", { name: "Forwards" });
    const row = within(table).getByText("postgres").closest("tr")!;
    expect(within(row).getByRole("link", { name: "postgres" })).toHaveAttribute("href", "/services/svc_pg001");
    expect(row).toHaveTextContent("2.0 KiB");
    expect(row).toHaveTextContent("4.0 KiB");
  });

  it("the client detail has no Forwards table without forwards", async () => {
    renderApp(<Routes><Route path="/clients/:id" element={<ClientDetail />} /></Routes>, `/clients/${db.clients[0].session_id}`);
    await screen.findByRole("table", { name: "Services" });
    expect(screen.queryByRole("table", { name: "Forwards" })).toBeNull();
  });

  it("the client list counts a session that only forwards", async () => {
    Object.assign(db.clients[0], { service_count: 0, services: [], forward_count: 2, forwards: [
      { id: "a", service_id: "svc_pg001", service: "postgres", since: since(), bytes_in: 1, bytes_out: 1 },
      { id: "b", service_id: "svc_pg001", service: "postgres", since: since(), bytes_in: 1, bytes_out: 1 },
    ] });
    renderApp(<Clients />, "/clients");
    expect(await screen.findByText("2 forwards")).toHaveAttribute("href", `/clients/${db.clients[0].session_id}`);
  });

  it("the client list links to the session that forwards, not the first one", async () => {
    Object.assign(db.clients[0], { service_count: 0, services: [], forward_count: 0, forwards: [] });
    db.clients.push({
      ...db.clients[0], session_id: "sess_forwarder", service_count: 0, services: [], forward_count: 1,
      forwards: [{ id: "a", service_id: "svc_pg001", service: "postgres", since: since(), bytes_in: 1, bytes_out: 1 }],
    });
    renderApp(<Clients />, "/clients");
    expect(await screen.findByText("1 forward")).toHaveAttribute("href", "/clients/sess_forwarder");
  });

  it("the client detail of a session that only forwards has no empty Services table", async () => {
    Object.assign(db.clients[0], { service_count: 0, services: [], forward_count: 1, forwards: [
      { id: "c_f1", service_id: "svc_pg001", service: "postgres", since: since(), bytes_in: 1, bytes_out: 1 },
    ] });
    renderApp(<Routes><Route path="/clients/:id" element={<ClientDetail />} /></Routes>, `/clients/${db.clients[0].session_id}`);
    await screen.findByRole("table", { name: "Forwards" });
    expect(screen.queryByRole("table", { name: "Services" })).toBeNull();
    expect(screen.queryByText("Online, but not serving any service yet.")).toBeNull();
  });

  it("a relay that omits forwards: the detail shows the services and the list counts them", async () => {
    const c = db.clients[0] as unknown as Record<string, unknown>;
    delete c.forwards;
    delete c.forward_count;
    renderApp(<Routes><Route path="/clients/:id" element={<ClientDetail />} /></Routes>, `/clients/${db.clients[0].session_id}`);
    const table = await screen.findByRole("table", { name: "Services" });
    expect(within(table).getByText("web-staging")).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "Forwards" })).toBeNull();
  });

  it("a relay that omits forwards: a session serving nothing shows no forward count", async () => {
    const c = db.clients[0] as unknown as Record<string, unknown>;
    delete c.forwards;
    delete c.forward_count;
    Object.assign(db.clients[0], { service_count: 0, services: [] });
    renderApp(<Clients />, "/clients");
    await screen.findByText("office-box-1");
    expect(screen.queryByText(/forwards?$/)).toBeNull();
  });
});
