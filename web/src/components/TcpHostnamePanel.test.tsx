import { describe, it, expect, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import { renderApp } from "@/mocks/test-utils";
import { resetDb, db, type ServiceRow } from "@/mocks/db";
import { TcpHostnamePanel } from "./TcpHostnamePanel";

function addTcp(): ServiceRow {
  const row: ServiceRow = {
    id: "svc_rdp01", user_id: db.me.id, name: "rdp", type: "tcp", slug: "rdp1", url: "", access_mode: "open",
    api_key_header: "Authorization", gateway_only: false, connected: true, remote_port: 0,
    local_addr: "127.0.0.1:3389", last_seen: "2026-10-23T09:00:00Z", last_client: "office-box-1",
  };
  db.services.push(row);
  return row;
}
const card = () => screen.findByRole("region", { name: "Host name" });

beforeEach(() => resetDb());

describe("TcpHostnamePanel reachability", () => {
  it("says the service is reachable on :443 when the 443 mux serves it", async () => {
    addTcp();
    db.tcpShadow.svc_rdp01 = { reachable_on: ["https"] };
    renderApp(<TcpHostnamePanel serviceId="svc_rdp01" mode="open" />);
    const c = await card();
    expect(await within(c).findByText(/Reachable on :443/)).toBeInTheDocument();
  });

  it("lists the dedicated listener beside :443", async () => {
    addTcp();
    db.tcpShadow.svc_rdp01 = { reachable_on: ["https", ":8444"] };
    renderApp(<TcpHostnamePanel serviceId="svc_rdp01" mode="open" />);
    const c = await card();
    const line = await within(c).findByText(/Reachable on/);
    expect(line).toHaveTextContent(":443");
    expect(line).toHaveTextContent(":8444");
  });

  it("an older relay without reachable_on says nothing about ports", async () => {
    addTcp();
    renderApp(<TcpHostnamePanel serviceId="svc_rdp01" mode="open" />);
    const c = await card();
    await within(c).findByText("Custom host name");
    expect(within(c).queryByText(/Reachable on/)).toBeNull();
  });

  it("warns when a custom domain of an http service wins the name", async () => {
    addTcp();
    db.tcpShadow.svc_rdp01 = { reachable_on: [], shadowed_by: "custom_domain" };
    renderApp(<TcpHostnamePanel serviceId="svc_rdp01" mode="open" />);
    const c = await card();
    expect(await within(c).findByRole("alert")).toHaveTextContent(/custom domain of an http service/i);
    expect(c).toHaveTextContent(/not reached on :443/i);
  });

  it("warns when the relay serves the name itself", async () => {
    addTcp();
    db.tcpShadow.svc_rdp01 = { reachable_on: ["https"], shadowed_by: "relay" };
    renderApp(<TcpHostnamePanel serviceId="svc_rdp01" mode="open" />);
    const c = await card();
    expect(await within(c).findByRole("alert")).toHaveTextContent(/relay serves .* itself/i);
  });
});
