import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, type ServiceRow } from "@/mocks/db";
import ServiceDetail from "@/pages/ServiceDetail";

function addTcp(over: Partial<ServiceRow> = {}): ServiceRow {
  const row: ServiceRow = {
    id: "svc_rdp01", user_id: db.me.id, name: "rdp", type: "tcp", slug: "", url: "", access_mode: "private",
    api_key_header: "Authorization", gateway_only: false, connected: true, remote_port: 0,
    local_addr: "127.0.0.1:3389", last_seen: "2026-10-23T09:00:00Z", last_client: "office-box-1", ...over,
  };
  db.services.push(row);
  return row;
}

function mount(id = "svc_rdp01") {
  return renderApp(
    <Routes><Route path="/services/:id" element={<ServiceDetail />} /></Routes>,
    `/services/${id}`,
  );
}

type Spy = { mock: { calls: unknown[][] } };
function bodies(spy: Spy, suffix: string, method: string) {
  return spy.mock.calls
    .filter(([url, init]) => String(url).endsWith(suffix) && (init as RequestInit | undefined)?.method === method)
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)));
}

describe("ServiceDetail of a tcp service", () => {
  afterEach(() => vi.restoreAllMocks());

  it("offers the three tcp modes and none of the http tabs", async () => {
    addTcp();
    mount();
    const modes = await screen.findByRole("radiogroup", { name: /access mode/i });
    expect(within(modes).getAllByRole("radio").map((r) => r.querySelector(".title")?.textContent))
      .toEqual(["Open", "IP allowlist", "Private"]);
    expect(within(modes).getByRole("radio", { name: "Private" })).toBeChecked();
    for (const gone of ["API keys", "Upstream key"]) {
      expect(screen.queryByRole("tab", { name: gone })).toBeNull();
    }
    expect(screen.queryByText(/Edit URL/)).toBeNull();
  });

  it("warns in red when Open is chosen, and says the port is risky for 3389, 22, 5900 and 5432", async () => {
    addTcp();
    mount();
    await userEvent.click(await screen.findByRole("radio", { name: "Open" }));
    const banner = screen.getByRole("alert");
    expect(banner).toHaveTextContent(/anyone on the internet/i);
    expect(banner).toHaveTextContent(/3389/);
    expect(banner).toHaveTextContent(/Remote Desktop/);
  });

  it("does not name a risky port for another local port", async () => {
    addTcp({ local_addr: "127.0.0.1:8080" });
    mount();
    await userEvent.click(await screen.findByRole("radio", { name: "Open" }));
    const banner = screen.getByRole("alert");
    expect(banner).toHaveTextContent(/anyone on the internet/i);
    expect(banner).not.toHaveTextContent(/Remote Desktop|3389|SSH|VNC|Postgres/);
  });

  it("saves the mode and says the client reconnects", async () => {
    addTcp();
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("radio", { name: "IP allowlist" }));
    expect(screen.getByText(/disconnects the client, which connects again by itself/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save mode" }));
    await waitFor(() => expect(bodies(spy, "/services/svc_rdp01/access-mode", "PUT")).toEqual([{ access_mode: "ip" }]));
    expect((await screen.findAllByText("IP allowlist")).length).toBeGreaterThan(1);
  });

  it("shows a refused mode change", async () => {
    addTcp();
    server.use(http.put("/api/v1/services/svc_rdp01/access-mode", () =>
      HttpResponse.json({ error: "a tcp service takes the access modes open, ip and private" }, { status: 409 })));
    mount();
    await userEvent.click(await screen.findByRole("radio", { name: "Open" }));
    await userEvent.click(screen.getByRole("button", { name: "Save mode" }));
    expect(await screen.findByText(/takes the access modes open, ip and private/)).toBeInTheDocument();
  });

  describe("connect instructions", () => {
    it("private: the forward command and the RDP steps", async () => {
      addTcp();
      mount();
      const card = await screen.findByRole("region", { name: "Connect" });
      expect(within(card).getByText("burrow forward rdp 13389")).toBeInTheDocument();
      expect(within(card).getByText("localhost:13389")).toBeInTheDocument();
      expect(card).toHaveTextContent(/mstsc/);
      expect(card).toHaveTextContent(/no public port/i);
    });

    it("open: the public address", async () => {
      addTcp({ access_mode: "open", remote_port: 9005 });
      mount();
      const card = await screen.findByRole("region", { name: "Connect" });
      expect(await within(card).findByText("relay.example.com:9005")).toBeInTheDocument();
      expect(card).not.toHaveTextContent(/burrow forward/);
    });
  });

  describe("permanent addresses (ip mode)", () => {
    it("takes a bare address and lists it, with no country fields", async () => {
      addTcp({ access_mode: "ip", remote_port: 9005 });
      const spy = vi.spyOn(globalThis, "fetch");
      mount();
      const panel = await screen.findByRole("region", { name: "Allowed addresses" });
      expect(panel).not.toHaveTextContent(/countr|geo/i);
      await userEvent.click(within(panel).getByRole("button", { name: "Add address" }));
      await userEvent.type(await screen.findByLabelText("Address or range"), "203.0.113.9");
      await userEvent.click(screen.getByRole("button", { name: "Add" }));
      await waitFor(() => expect(bodies(spy, "/services/svc_rdp01/ip-geo", "PUT")).toHaveLength(1));
      expect(bodies(spy, "/services/svc_rdp01/ip-geo", "PUT")[0].allow_cidrs).toEqual(["203.0.113.9"]);
      expect(await within(panel).findByText(/allow 203\.0\.113\.9/)).toBeInTheDocument();
    });

    it("refuses text that is no address", async () => {
      addTcp({ access_mode: "ip", remote_port: 9005 });
      mount();
      const panel = await screen.findByRole("region", { name: "Allowed addresses" });
      await userEvent.click(within(panel).getByRole("button", { name: "Add address" }));
      await userEvent.type(await screen.findByLabelText("Address or range"), "not-an-ip");
      await userEvent.click(screen.getByRole("button", { name: "Add" }));
      expect(await screen.findByText(/not an address or range/i)).toBeInTheDocument();
    });

    it("says nobody gets in while the list and the grants are empty", async () => {
      addTcp({ access_mode: "ip", remote_port: 9005 });
      mount();
      const panel = await screen.findByRole("region", { name: "Allowed addresses" });
      expect(panel).toHaveTextContent(/nobody can connect/i);
    });

    it("is not shown for another mode", async () => {
      addTcp();
      mount();
      await screen.findByRole("radiogroup", { name: /access mode/i });
      expect(screen.queryByRole("region", { name: "Allowed addresses" })).toBeNull();
      expect(screen.queryByRole("region", { name: "Allow my address" })).toBeNull();
    });
  });

  describe("just-in-time grants", () => {
    function mountIp() {
      addTcp({ access_mode: "ip", remote_port: 9005 });
      return mount();
    }

    it("allows my address for 1, 8 or 24 hours and shows what the relay detected", async () => {
      const spy = vi.spyOn(globalThis, "fetch");
      mountIp();
      const panel = await screen.findByRole("region", { name: "Allow my address" });
      for (const h of ["1 h", "8 h", "24 h"]) {
        expect(within(panel).getByRole("button", { name: `Allow my IP for ${h}` })).toBeInTheDocument();
      }
      await userEvent.click(within(panel).getByRole("button", { name: "Allow my IP for 8 h" }));
      await waitFor(() => expect(bodies(spy, "/services/svc_rdp01/ip-grants", "POST")).toEqual([{ ttl_hours: 8 }]));
      const detected = await within(panel).findByRole("status");
      expect(detected).toHaveTextContent("203.0.113.7/32");
      expect(detected).toHaveTextContent("IPv4");
      const list = within(panel).getByRole("list", { name: "Active grants" });
      const item = within(list).getByText("203.0.113.7/32").closest("li")!;
      expect(item).toHaveTextContent(/7 h 59 min left|8 h left/);
    });

    it("names an IPv6 address and widens it to its /64", async () => {
      db.callerIp = "2001:db8:1:2:3:4:5:6";
      const spy = vi.spyOn(globalThis, "fetch");
      mountIp();
      const panel = await screen.findByRole("region", { name: "Allow my address" });
      await userEvent.click(within(panel).getByRole("button", { name: "Allow my IP for 1 h" }));
      const detected = await within(panel).findByRole("status");
      expect(detected).toHaveTextContent("IPv6");
      await userEvent.click(within(detected).getByRole("button", { name: "Widen to 2001:db8:1:2::/64" }));
      await waitFor(() => expect(bodies(spy, "/services/svc_rdp01/ip-grants", "POST")).toHaveLength(2));
      expect(bodies(spy, "/services/svc_rdp01/ip-grants", "POST")[1]).toEqual({ cidr: "2001:db8:1:2::/64", ttl_hours: 1 });
      expect(await within(await within(panel).findByRole("list", { name: "Active grants" })).findByText("2001:db8:1:2::/64")).toBeInTheDocument();
    });

    it("widens an IPv4 address to its /24 for the same time", async () => {
      const spy = vi.spyOn(globalThis, "fetch");
      mountIp();
      const panel = await screen.findByRole("region", { name: "Allow my address" });
      await userEvent.click(within(panel).getByRole("button", { name: "Allow my IP for 24 h" }));
      const detected = await within(panel).findByRole("status");
      await userEvent.click(within(detected).getByRole("button", { name: "Widen to 203.0.113.0/24" }));
      await waitFor(() => expect(bodies(spy, "/services/svc_rdp01/ip-grants", "POST")).toHaveLength(2));
      expect(bodies(spy, "/services/svc_rdp01/ip-grants", "POST")[1]).toEqual({ cidr: "203.0.113.0/24", ttl_hours: 24 });
    });

    it("takes a manual address with its own time", async () => {
      const spy = vi.spyOn(globalThis, "fetch");
      mountIp();
      const panel = await screen.findByRole("region", { name: "Allow my address" });
      await userEvent.type(within(panel).getByLabelText("Another address"), "198.51.100.4");
      await userEvent.selectOptions(within(panel).getByLabelText("For"), "24");
      await userEvent.click(within(panel).getByRole("button", { name: "Allow address" }));
      await waitFor(() => expect(bodies(spy, "/services/svc_rdp01/ip-grants", "POST"))
        .toEqual([{ cidr: "198.51.100.4", ttl_hours: 24 }]));
      expect(await within(await within(panel).findByRole("list", { name: "Active grants" })).findByText("198.51.100.4/32")).toBeInTheDocument();
    });

    it("passes on the relay's warning about a private address", async () => {
      server.use(http.post("/api/v1/services/svc_rdp01/ip-grants", () => HttpResponse.json({
        id: "g1", cidr: "172.18.0.1/32", family: "ipv4", expires_at: new Date(Date.now() + 3600_000).toISOString(),
        note: "", created_by: "u", created_at: new Date().toISOString(), from_caller: true,
        warning: "172.18.0.1 is a private address: the relay may see a proxy instead of the visitor",
      }, { status: 201 })));
      mountIp();
      const panel = await screen.findByRole("region", { name: "Allow my address" });
      await userEvent.click(within(panel).getByRole("button", { name: "Allow my IP for 1 h" }));
      expect(await within(panel).findByText(/may see a proxy instead of the visitor/)).toBeInTheDocument();
    });

    it("revokes a grant", async () => {
      const spy = vi.spyOn(globalThis, "fetch");
      db.ipGrants.svc_rdp01 = [{
        id: "g9", cidr: "198.51.100.0/24", family: "ipv4", expires_at: new Date(Date.now() + 90 * 60_000).toISOString(),
        note: "", created_by: db.me.id, created_at: new Date().toISOString(),
      }];
      mountIp();
      const list = await screen.findByRole("list", { name: "Active grants" });
      expect(within(list).getByText("198.51.100.0/24")).toBeInTheDocument();
      expect(list).toHaveTextContent(/1 h 29 min left|1 h 30 min left/);
      await userEvent.click(within(list).getByRole("button", { name: "Revoke 198.51.100.0/24" }));
      await waitFor(() => expect(spy.mock.calls.some(([u, i]) =>
        String(u).endsWith("/services/svc_rdp01/ip-grants/g9") && (i as RequestInit)?.method === "DELETE")).toBe(true));
      await waitFor(() => expect(screen.queryByText("198.51.100.0/24")).toBeNull());
      expect(await screen.findByText(/No grants/)).toBeInTheDocument();
    });

    it("shows the relay's refusal", async () => {
      server.use(http.post("/api/v1/services/svc_rdp01/ip-grants", () =>
        HttpResponse.json({ error: "the service has 32 grants" }, { status: 409 })));
      mountIp();
      const panel = await screen.findByRole("region", { name: "Allow my address" });
      await userEvent.click(within(panel).getByRole("button", { name: "Allow my IP for 1 h" }));
      expect(await within(panel).findByRole("alert")).toHaveTextContent("the service has 32 grants");
    });
  });

  describe("forward policy", () => {
    it("says that an empty policy means the owner only", async () => {
      addTcp();
      mount();
      const panel = await screen.findByRole("region", { name: "Who may forward" });
      expect(await within(panel).findByText(/Empty policy: only the owner can forward to this service/)).toBeInTheDocument();
      expect(panel).not.toHaveTextContent(/nobody can reach/i);
    });

    it("saves the roles", async () => {
      addTcp();
      mount();
      const panel = await screen.findByRole("region", { name: "Who may forward" });
      await userEvent.click(await within(panel).findByRole("button", { name: /^user$/ }));
      await userEvent.click(within(panel).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(db.serviceAccessPolicy.svc_rdp01).toEqual(["user"]));
    });
  });

  describe("connections", () => {
    it("lists the service's connection log, refused ones too", async () => {
      addTcp({ access_mode: "ip", remote_port: 9005 });
      const base = { kind: "tcp_proxy" as const, service_id: "svc_rdp01", tunnel_id: "t", user_id: "", client_session_id: "s", user_agent: "", ended_at: "2026-10-23T09:05:00Z", reason: "" };
      db.connectionLogs.push(
        { ...base, id: "cl_a", source_ip: "198.51.100.77", started_at: "2026-10-23T09:00:00Z", duration_ms: 300000, bytes_in: 2048, bytes_out: 4096, status: "closed_clean" },
        { ...base, id: "cl_b", source_ip: "192.0.2.66", started_at: "2026-10-23T09:06:00Z", duration_ms: 0, bytes_in: 0, bytes_out: 0, status: "rejected", reason: "ip_geo" },
        { ...base, id: "cl_c", service_id: "svc_other", source_ip: "10.9.9.9", started_at: "2026-10-23T09:07:00Z", duration_ms: 1, bytes_in: 0, bytes_out: 0, status: "closed_clean" },
      );
      mount();
      const panel = await screen.findByRole("region", { name: "Recent connections" });
      expect(await within(panel).findByText("198.51.100.77")).toBeInTheDocument();
      expect(within(panel).getByText("192.0.2.66")).toBeInTheDocument();
      expect(within(panel).getByText(/ip_geo/)).toBeInTheDocument();
      expect(within(panel).queryByText("10.9.9.9")).toBeNull();
      expect(within(panel).getByRole("link", { name: /open the traffic log/i }))
        .toHaveAttribute("href", "/traffic?service=svc_rdp01");
      // The relay lists a connection once it has ended: the panel says so.
      expect(panel).toHaveTextContent(/appear here when they end/i);
    });

    it("says what it cannot read without audit access", async () => {
      addTcp();
      server.use(http.get("/api/v1/connection-logs", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
      mount();
      const panel = await screen.findByRole("region", { name: "Recent connections" });
      expect(await within(panel).findByText(/needs audit access/i)).toBeInTheDocument();
    });

    it("shows the live traffic of the connected service", async () => {
      addTcp({ access_mode: "ip", remote_port: 9005 });
      db.clients[0].services.push({ id: "tnl_rdp", name: "rdp", type: "tcp", remote_port: 9005, local_addr: "127.0.0.1:3389", access_mode: "ip", bytes_in: 3072, bytes_out: 5120, total_bytes_in: 3072, total_bytes_out: 5120 });
      mount();
      const panel = await screen.findByRole("region", { name: "Recent connections" });
      const live = await within(panel).findByLabelText("Live traffic");
      expect(live).toHaveTextContent("3.0 KiB");
      expect(live).toHaveTextContent("5.0 KiB");
    });
  });
});
