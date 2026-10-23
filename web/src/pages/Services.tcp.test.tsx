import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { db, type ServiceRow } from "@/mocks/db";
import Services from "@/pages/Services";

function addTcp(over: Partial<ServiceRow>): ServiceRow {
  const row: ServiceRow = {
    id: "svc_rdp01", user_id: db.me.id, name: "rdp", type: "tcp", slug: "", url: "", access_mode: "private",
    api_key_header: "Authorization", gateway_only: false, connected: true, remote_port: 0,
    local_addr: "127.0.0.1:3389", last_seen: "2026-10-23T09:00:00Z", last_client: "office-box-1", ...over,
  };
  db.services.push(row);
  return row;
}

describe("Services list: tcp services", () => {
  afterEach(() => vi.restoreAllMocks());

  it("shows the address a visitor uses and an access badge for each mode", async () => {
    addTcp({});
    addTcp({ id: "svc_ssh01", name: "ssh-box", access_mode: "ip", remote_port: 9007, local_addr: "127.0.0.1:22" });
    addTcp({ id: "svc_db01", name: "dbtcp", access_mode: "open", remote_port: 9008, local_addr: "127.0.0.1:5432" });
    renderApp(<Services />, "/services");
    const table = await screen.findByRole("table", { name: "Services" });

    const priv = within(table).getByText("rdp").closest("tr")!;
    expect(within(priv).getByText("burrow forward rdp 13389")).toBeInTheDocument();
    expect(within(priv).getByText("Private")).toBeInTheDocument();
    expect(within(priv).getByRole("button", { name: "Copy command burrow forward rdp 13389" })).toBeInTheDocument();

    const ip = within(table).getByText("ssh-box").closest("tr")!;
    expect(within(ip).getByText("IP allowlist")).toBeInTheDocument();
    expect(await within(ip).findByText("relay.example.com:9007")).toBeInTheDocument();
    expect(within(ip).getByRole("button", { name: "Copy address relay.example.com:9007" })).toBeInTheDocument();

    const open = within(table).getByText("dbtcp").closest("tr")!;
    expect(within(open).getByText("Open")).toBeInTheDocument();
    expect(await within(open).findByText("relay.example.com:9008")).toBeInTheDocument();
  });

  it("leaves the address empty while an open service has no port", async () => {
    addTcp({ access_mode: "open", connected: false, remote_port: 0 });
    renderApp(<Services />, "/services");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByText("rdp").closest("tr")!;
    expect(within(row).queryByRole("button", { name: /copy address/i })).toBeNull();
  });

  it("opens a tcp service's own page to configure it, not the http dialog", async () => {
    addTcp({});
    renderApp(<Services />, "/services");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByText("rdp").closest("tr")!;
    const link = within(row).getByRole("link", { name: "Configure" });
    expect(link).toHaveAttribute("href", "/services/svc_rdp01");
    await userEvent.click(link);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("joins a live tcp connection to its service by name, and shows no port for a private one", async () => {
    addTcp({});
    db.clients[0].services.push({ id: "tnl_rdp", name: "rdp", type: "tcp", remote_port: 0, local_addr: "127.0.0.1:3389", access_mode: "private", bytes_in: 10, bytes_out: 20, total_bytes_in: 10, total_bytes_out: 20 });
    renderApp(<Services />, "/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = (await within(table).findByRole("link", { name: "rdp" })).closest("tr")!;
    expect(within(row).getByText("Private")).toBeInTheDocument();
    expect(within(row).queryByText(":0")).toBeNull();
    expect(within(row).getByRole("link", { name: "Configure" })).toHaveAttribute("href", "/services/svc_rdp01");
  });

  it("leaves a live tcp connection unjoined when two services share its name", async () => {
    addTcp({});
    addTcp({ id: "svc_rdp02", user_id: "someone_else" });
    db.clients[0].services.push({ id: "tnl_rdp", name: "rdp", type: "tcp", remote_port: 0, local_addr: "127.0.0.1:3389", access_mode: "private", bytes_in: 10, bytes_out: 20, total_bytes_in: 10, total_bytes_out: 20 });
    renderApp(<Services />, "/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = (await within(table).findByText("rdp")).closest("tr")!;
    expect(within(row).queryByRole("link", { name: "rdp" })).toBeNull();
    expect(within(row).queryByRole("link", { name: "Configure" })).toBeNull();
  });

  it("writes the forward command with the slug when the service has one", async () => {
    addTcp({ slug: "rdp-x7k2" });
    renderApp(<Services />, "/services");
    const table = await screen.findByRole("table", { name: "Services" });
    const row = within(table).getByText("rdp").closest("tr")!;
    expect(within(row).getByText("burrow forward rdp-x7k2 13389")).toBeInTheDocument();
  });

  it("joins a live tcp connection by the service id it names, even when two services share the name", async () => {
    addTcp({});
    addTcp({ id: "svc_rdp02", user_id: "someone_else" });
    server.use(http.get("/api/v1/tunnels", () => HttpResponse.json([
      { id: "t1", name: "rdp", type: "tcp", remote_port: 0, local_addr: "127.0.0.1:3389", bytes_in: 1, bytes_out: 2, connected: true, service_id: "svc_rdp02", access_mode: "private" },
    ])));
    renderApp(<Services />, "/services?live=1");
    const table = await screen.findByRole("table", { name: "Services" });
    const link = await within(table).findByRole("link", { name: "rdp" });
    expect(link).toHaveAttribute("href", "/services/svc_rdp02");
  });
});
