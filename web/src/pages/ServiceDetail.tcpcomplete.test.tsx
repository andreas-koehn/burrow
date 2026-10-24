import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, type ServiceRow } from "@/mocks/db";
import ServiceDetail from "@/pages/ServiceDetail";
import { ReauthDialog } from "@/components/ReauthDialog";

// tcp complete (E1-E8): every setting of a tcp service on its page.

function addTcp(over: Partial<ServiceRow> = {}): ServiceRow {
  const row: ServiceRow = {
    id: "svc_rdp01", user_id: db.me.id, name: "rdp", type: "tcp", slug: "k7p2qx", url: "", access_mode: "open",
    api_key_header: "Authorization", gateway_only: false, connected: true, remote_port: 9005,
    local_addr: "127.0.0.1:3389", last_seen: "2026-10-23T09:00:00Z", last_client: "office-box-1", ...over,
  };
  db.services.push(row);
  return row;
}

function mount(id = "svc_rdp01") {
  return renderApp(<Routes><Route path="/services/:id" element={<ServiceDetail />} /></Routes>, `/services/${id}`);
}

type Spy = { mock: { calls: unknown[][] } };
function sent(spy: Spy, suffix: string, method: string) {
  return spy.mock.calls
    .filter(([url, init]) => String(url).includes(suffix) && (init as RequestInit | undefined)?.method === method)
    .map(([, init]) => (init as RequestInit).body ? JSON.parse(String((init as RequestInit).body)) : null);
}
const region = (name: string) => screen.findByRole("region", { name });

describe("tcp service page: connection settings", () => {
  afterEach(() => vi.restoreAllMocks());

  it("turns the PROXY protocol and the idle timeout on with one save of what changed", async () => {
    addTcp();
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Connection settings");
    await userEvent.click(within(card).getByRole("switch", { name: /PROXY protocol/i }));
    const idle = within(card).getByLabelText("Idle timeout (minutes)");
    await userEvent.clear(idle);
    await userEvent.type(idle, "15");
    await userEvent.click(within(card).getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(sent(spy, "/tcp-settings", "PUT")).toEqual([{ proxy_protocol: "v2", idle_timeout_s: 900 }]));
    expect(db.tcpSettings.svc_rdp01).toMatchObject({ proxy_protocol: "v2", idle_timeout_s: 900 });
  });

  it("sends only the field that changed, and 0 for never", async () => {
    addTcp();
    db.tcpSettings.svc_rdp01 = { proxy_protocol: "v2", idle_timeout_s: 600, client_proxy_protocol: "", client_proxy_supported: true };
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Connection settings");
    const idle = await within(card).findByLabelText("Idle timeout (minutes)");
    await waitFor(() => expect(idle).toHaveValue(10));
    await userEvent.clear(idle);
    await userEvent.type(idle, "0");
    await userEvent.click(within(card).getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(sent(spy, "/tcp-settings", "PUT")).toEqual([{ idle_timeout_s: 0 }]));
  });

  it("warns when the serving client cannot write the header", async () => {
    addTcp();
    db.tcpSettings.svc_rdp01 = { proxy_protocol: "v2", idle_timeout_s: 0, client_proxy_protocol: "", client_proxy_supported: false };
    mount();
    const card = await region("Connection settings");
    expect(await within(card).findByText(/too old to send the PROXY header/i)).toBeInTheDocument();
    expect(card).toHaveTextContent(/refuses its connections/i);
  });

  it("does not warn while the client can write it, or none is online", async () => {
    addTcp();
    db.tcpSettings.svc_rdp01 = { proxy_protocol: "v2", idle_timeout_s: 0, client_proxy_protocol: "v2", client_proxy_supported: null };
    mount();
    const card = await region("Connection settings");
    expect(await within(card).findByText(/also sends it/i)).toBeInTheDocument();
    expect(card).not.toHaveTextContent(/too old/i);
  });

  it("shows the relay's refusal of a value", async () => {
    addTcp();
    server.use(http.put("/api/v1/services/svc_rdp01/tcp-settings", () =>
      HttpResponse.json({ error: "idle_timeout_s must be 0 (never) or 60 to 86400" }, { status: 400 })));
    mount();
    const card = await region("Connection settings");
    const idle = await within(card).findByLabelText("Idle timeout (minutes)");
    await userEvent.type(idle, "2000");
    await userEvent.click(within(card).getByRole("button", { name: "Save settings" }));
    expect(await within(card).findByText(/60 to 86400/)).toBeInTheDocument();
  });

  it("is read only without services:configure", async () => {
    addTcp();
    db.me = { id: db.me.id, email: "bob@acme.io", role: "operator", permissions: ["tunnels:read:own"] };
    mount();
    await screen.findByRole("region", { name: "Access mode" });
    expect(screen.queryByRole("region", { name: "Connection settings" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Limits" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Bans" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Host name" })).toBeNull();
  });
});

describe("tcp service page: host name", () => {
  afterEach(() => vi.restoreAllMocks());

  it("shows the slug name and, for a holder of services:configure:any, sets a custom one", async () => {
    addTcp();
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Host name");
    expect(await within(card).findByText("k7p2qx.tcp.example.com")).toBeInTheDocument();
    await userEvent.type(within(card).getByLabelText("Custom host name"), "rdp.example.org");
    await userEvent.click(within(card).getByRole("button", { name: "Save host name" }));
    await waitFor(() => expect(sent(spy, "/tcp-hostname", "PUT")).toEqual([{ tcp_hostname: "rdp.example.org" }]));
    expect(await within(card).findByText("rdp.example.org")).toBeInTheDocument();
  });

  it("clears the custom name", async () => {
    addTcp();
    db.tcpHostnames.svc_rdp01 = "rdp.example.org";
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Host name");
    await waitFor(() => expect(within(card).getByLabelText("Custom host name")).toHaveValue("rdp.example.org"));
    await userEvent.click(within(card).getByRole("button", { name: "Remove custom name" }));
    await waitFor(() => expect(sent(spy, "/tcp-hostname", "PUT")).toEqual([{ tcp_hostname: "" }]));
  });

  it("offers no custom field to an owner without :any, who still sees the slug name", async () => {
    addTcp();
    db.me = { id: db.me.id, email: "bob@acme.io", role: "operator", permissions: ["tunnels:read:own", "services:configure:own"] };
    mount();
    const card = await region("Host name");
    expect(await within(card).findByText("k7p2qx.tcp.example.com")).toBeInTheDocument();
    expect(within(card).queryByLabelText("Custom host name")).toBeNull();
    expect(card).toHaveTextContent(/services:configure:any/);
  });

  it("says a private service has no name on the passthrough listener, and when the relay has none", async () => {
    addTcp({ access_mode: "private", remote_port: 0 });
    db.tlsPassthrough = false;
    mount();
    const card = await region("Host name");
    expect(await within(card).findByText(/no TLS passthrough listener/i)).toBeInTheDocument();
    expect(card).toHaveTextContent(/private/i);
  });

  it("shows a taken name at the field", async () => {
    addTcp();
    server.use(http.put("/api/v1/services/svc_rdp01/tcp-hostname", () =>
      HttpResponse.json({ error: "tcp_hostname is used by another service" }, { status: 409 })));
    mount();
    const card = await region("Host name");
    await userEvent.type(await within(card).findByLabelText("Custom host name"), "rdp.example.org");
    await userEvent.click(within(card).getByRole("button", { name: "Save host name" }));
    expect(await within(card).findByText(/used by another service/)).toBeInTheDocument();
  });
});

describe("tcp service page: host name only", () => {
  afterEach(() => vi.restoreAllMocks());

  it("turns the public port off while a name reaches the service, and on again", async () => {
    addTcp();
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Host name");
    const sw = await within(card).findByRole("switch", { name: "Host name only" });
    expect(sw).toHaveAttribute("aria-checked", "false");
    await userEvent.click(sw);
    await waitFor(() => expect(sent(spy, "/tcp-hostname", "PUT")).toEqual([{ hostname_only: true }]));
    expect(db.tcpHostnameOnly.svc_rdp01).toBe(true);
    await waitFor(() => expect(within(card).getByRole("switch", { name: "Host name only" })).toHaveAttribute("aria-checked", "true"));
    expect(card).toHaveTextContent(/no public port/i);
    await userEvent.click(within(card).getByRole("switch", { name: "Host name only" }));
    await waitFor(() => expect(sent(spy, "/tcp-hostname", "PUT")).toEqual([{ hostname_only: true }, { hostname_only: false }]));
    expect(db.tcpHostnameOnly.svc_rdp01).toBe(false);
  });

  it("says the port is open again when the service lost its only name", async () => {
    addTcp({ slug: "" });
    db.tcpHostnameOnly.svc_rdp01 = true;
    mount();
    const card = await region("Host name");
    await within(card).findByRole("switch", { name: "Host name only" });
    expect(card).toHaveTextContent(/public port is open again/i);
    expect(card).toHaveTextContent(/no host name/i);
  });

  it("cannot turn the port off while no name reaches the service", async () => {
    addTcp({ slug: "" });
    mount();
    const card = await region("Host name");
    const sw = await within(card).findByRole("switch", { name: "Host name only" });
    expect(sw).toBeDisabled();
    expect(card).toHaveTextContent(/needs a host name/i);
  });

  it("shows the relay's refusal", async () => {
    addTcp();
    server.use(http.put("/api/v1/services/svc_rdp01/tcp-hostname", () =>
      HttpResponse.json({ error: "hostname_only needs the relay's TLS passthrough listener" }, { status: 400 })));
    mount();
    const card = await region("Host name");
    await userEvent.click(await within(card).findByRole("switch", { name: "Host name only" }));
    expect(await within(card).findByText(/needs the relay's TLS passthrough listener/)).toBeInTheDocument();
  });

  it("lets a service that has no way in by name back onto its port", async () => {
    addTcp();
    db.tlsPassthrough = false;
    db.tcpHostnameOnly.svc_rdp01 = true;
    mount();
    const card = await region("Host name");
    const sw = await within(card).findByRole("switch", { name: "Host name only" });
    expect(sw).toBeEnabled();
    // The relay opened the port again: the card says so, and why.
    expect(card).toHaveTextContent(/public port is open again/i);
    expect(card).toHaveTextContent(/no TLS passthrough listener/i);
    expect(card).not.toHaveTextContent(/has no public port/i);
    await userEvent.click(sw);
    await waitFor(() => expect(db.tcpHostnameOnly.svc_rdp01).toBe(false));
  });
});

describe("tcp service page: countries", () => {
  afterEach(() => vi.restoreAllMocks());

  it("adds an allowed country in mode ip and lists it", async () => {
    addTcp({ access_mode: "ip" });
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Countries");
    await userEvent.click(within(card).getByRole("combobox", { name: "List" }));
    await userEvent.click(await screen.findByRole("option", { name: "Allow" }));
    await userEvent.type(await within(card).findByLabelText("Country code"), "de");
    await userEvent.click(within(card).getByRole("button", { name: "Add country" }));
    await waitFor(() => expect(sent(spy, "/ip-geo", "PUT")[0]).toMatchObject({ allow_countries: ["DE"], block_countries: [] }));
    expect(await within(card).findByText(/allow DE/)).toBeInTheDocument();
  });

  it("blocks a country in mode open, and removes it again", async () => {
    addTcp({ access_mode: "open" });
    db.tcpIpGeo.svc_rdp01 = { enabled: true, allow_cidrs: [], block_cidrs: [], allow_countries: [], block_countries: ["RU"] };
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Countries");
    expect(await within(card).findByText(/block RU/)).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: "Remove country RU" }));
    await waitFor(() => expect(sent(spy, "/ip-geo", "PUT")[0]).toMatchObject({ block_countries: [] }));
  });

  it("says the lists are not enforced without a database, and refuses new countries", async () => {
    addTcp({ access_mode: "ip" });
    db.geoDatabase = false;
    db.tcpIpGeo.svc_rdp01 = { enabled: true, allow_cidrs: [], block_cidrs: [], allow_countries: ["DE"], block_countries: [] };
    mount();
    const card = await region("Countries");
    expect(await within(card).findByText(/not enforced/i)).toBeInTheDocument();
    expect(card).toHaveTextContent(/BURROW_GEOIP_DB/);
    await waitFor(() => expect(within(card).getByRole("button", { name: "Add country" })).toBeDisabled());
  });

  it("explains an unknown country", async () => {
    addTcp({ access_mode: "open" });
    mount();
    const card = await region("Countries");
    expect(await within(card).findByText(/country is unknown/i)).toBeInTheDocument();
  });

  it("is not offered in mode private, nor without ipgeo:manage", async () => {
    addTcp({ access_mode: "private", remote_port: 0 });
    mount();
    await screen.findByRole("region", { name: "Connect" });
    expect(screen.queryByRole("region", { name: "Countries" })).toBeNull();
  });

  it("is hidden for a caller without ipgeo:manage", async () => {
    addTcp({ access_mode: "open" });
    db.me = { id: db.me.id, email: "bob@acme.io", role: "operator", permissions: ["tunnels:read:own", "services:configure:own"] };
    mount();
    await screen.findByRole("region", { name: "Limits" });
    expect(screen.queryByRole("region", { name: "Countries" })).toBeNull();
  });
});

describe("tcp service page: limits", () => {
  afterEach(() => vi.restoreAllMocks());

  it("saves the bandwidth, the daily volume and the automatic bans in one request", async () => {
    addTcp();
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Limits");
    await userEvent.type(await within(card).findByLabelText("Bandwidth limit (kbit/s)"), "2000");
    await userEvent.type(within(card).getByLabelText("Daily volume (GiB)"), "5");
    await userEvent.click(within(card).getByRole("switch", { name: /automatic bans/i }));
    const count = within(card).getByLabelText("Connections");
    await userEvent.clear(count);
    await userEvent.type(count, "5");
    await userEvent.click(within(card).getByRole("button", { name: "Save limits" }));
    await waitFor(() => expect(sent(spy, "/tcp-limits", "PUT")).toEqual([{
      rate_limit_kbps: 2000, daily_bytes: 5 * 1024 ** 3,
      autoban: { enabled: true, short_s: 3, count: 5, window_min: 10, ban_min: 60 },
    }]));
  });

  it("shows today's usage as a bar", async () => {
    addTcp();
    db.tcpLimits.svc_rdp01 = {
      rate_limit_kbps: 0, daily_bytes: 4 * 1024 ** 3, quota_reached: false,
      autoban: { enabled: false, short_s: 3, count: 10, window_min: 10, ban_min: 60 },
      usage_today: { day: "2026-10-24", bytes_in: 1024 ** 3, bytes_out: 1024 ** 3, bytes: 2 * 1024 ** 3 },
    };
    mount();
    const card = await region("Limits");
    const bar = await within(card).findByRole("progressbar", { name: /today/i });
    expect(bar).toHaveAttribute("aria-valuenow", "50");
    expect(card).toHaveTextContent(/2\.0 GiB of 4\.0 GiB/);
  });

  it("says when the daily volume is used up", async () => {
    addTcp();
    db.tcpLimits.svc_rdp01 = {
      rate_limit_kbps: 0, daily_bytes: 1024 ** 3, quota_reached: true,
      autoban: { enabled: false, short_s: 3, count: 10, window_min: 10, ban_min: 60 },
      usage_today: { day: "2026-10-24", bytes_in: 1024 ** 3, bytes_out: 0, bytes: 1024 ** 3 },
    };
    mount();
    const card = await region("Limits");
    expect(await within(card).findByText(/refused until the next UTC day/i)).toBeInTheDocument();
    expect(card).toHaveTextContent(/forwards still connect/i);
  });

  it("shows no bar without a daily volume, and shows the ban defaults while off", async () => {
    addTcp();
    mount();
    const card = await region("Limits");
    await within(card).findByLabelText("Bandwidth limit (kbit/s)");
    expect(within(card).queryByRole("progressbar")).toBeNull();
    expect(within(card).getByLabelText("Window (minutes)")).toHaveValue(10);
    expect(within(card).getByLabelText("Ban for (minutes)")).toHaveValue(60);
    expect(card).toHaveTextContent(/forwards are never banned/i);
  });

  it("shows the relay's refusal", async () => {
    addTcp();
    mount();
    const card = await region("Limits");
    await userEvent.type(await within(card).findByLabelText("Bandwidth limit (kbit/s)"), "10");
    await userEvent.click(within(card).getByRole("button", { name: "Save limits" }));
    expect(await within(card).findByRole("alert")).toHaveTextContent(/64 to 10000000/);
  });
});

describe("tcp service page: bans", () => {
  afterEach(() => vi.restoreAllMocks());
  const soon = () => new Date(Date.now() + 3600_000).toISOString();

  it("lists the active bans, automatic ones marked", async () => {
    addTcp();
    db.tcpBans.svc_rdp01 = [
      { source: "198.51.100.7", until: soon(), reason: "auto: 10 short connections in 10 min", auto: true, created_at: "2026-10-24T08:00:00Z" },
      { source: "203.0.113.9", until: soon(), reason: "manual: scanner", auto: false, created_at: "2026-10-24T08:10:00Z" },
    ];
    mount();
    const card = await region("Bans");
    const rows = await within(card).findAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(rows[1]).toHaveTextContent("198.51.100.7");
    expect(rows[1]).toHaveTextContent(/automatic/i);
    expect(rows[2]).toHaveTextContent("203.0.113.9");
    expect(rows[2]).toHaveTextContent(/manual/i);
  });

  it("says no source is banned", async () => {
    addTcp();
    mount();
    const card = await region("Bans");
    expect(await within(card).findByText(/no source is banned/i)).toBeInTheDocument();
  });

  it("bans a source by hand", async () => {
    addTcp();
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Bans");
    await userEvent.type(await within(card).findByLabelText("Source address"), "198.51.100.20");
    const minutes = within(card).getByLabelText("Ban for (minutes)");
    await userEvent.clear(minutes);
    await userEvent.type(minutes, "120");
    await userEvent.type(within(card).getByLabelText("Note"), "scanner");
    await userEvent.click(within(card).getByRole("button", { name: "Ban source" }));
    await waitFor(() => expect(sent(spy, "/tcp-bans", "POST")).toEqual([{ source: "198.51.100.20", ban_min: 120, note: "scanner" }]));
    expect(await within(card).findByText("198.51.100.20")).toBeInTheDocument();
  });

  it("asks before it lifts a ban, and lifts it", async () => {
    addTcp();
    db.tcpBans.svc_rdp01 = [{ source: "198.51.100.7", until: soon(), reason: "auto", auto: true, created_at: "2026-10-24T08:00:00Z" }];
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Bans");
    await userEvent.click(await within(card).findByRole("button", { name: "Unban 198.51.100.7" }));
    expect(sent(spy, "/tcp-bans", "DELETE")).toEqual([]);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/198\.51\.100\.7/);
    await userEvent.click(within(dialog).getByRole("button", { name: "Unban" }));
    await waitFor(() => expect(spy.mock.calls.some(([u, i]) => String(u).includes("/tcp-bans?source=198.51.100.7") && (i as RequestInit)?.method === "DELETE")).toBe(true));
    await waitFor(() => expect(within(card).queryByText("198.51.100.7")).toBeNull());
  });

  it("keeps the ban when the confirmation is cancelled", async () => {
    addTcp();
    db.tcpBans.svc_rdp01 = [{ source: "198.51.100.7", until: soon(), reason: "auto", auto: true, created_at: "2026-10-24T08:00:00Z" }];
    mount();
    const card = await region("Bans");
    await userEvent.click(await within(card).findByRole("button", { name: "Unban 198.51.100.7" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(db.tcpBans.svc_rdp01).toHaveLength(1);
  });

  it("shows a refused ban at the form", async () => {
    addTcp();
    mount();
    const card = await region("Bans");
    await userEvent.type(await within(card).findByLabelText("Source address"), "not-an-ip");
    await userEvent.click(within(card).getByRole("button", { name: "Ban source" }));
    expect(await within(card).findByText(/source must be an IP address/)).toBeInTheDocument();
  });
});

describe("tcp service page: what a connection looked like", () => {
  afterEach(() => vi.restoreAllMocks());

  it("shows the protocol and the server name of live connections and of the recent ones", async () => {
    addTcp();
    db.serviceConnections.svc_rdp01 = [{
      id: "c1", kind: "visitor", source_ip: "198.51.100.7", user_id: "", user_email: "", token_name: "", session_id: "",
      since: new Date().toISOString(), bytes_in: 1, bytes_out: 2, protocol: "tls", sni: "rdp.example.org",
    }];
    mount();
    const live = await screen.findByRole("table", { name: "Open connections" });
    expect(await within(live).findByText("rdp.example.org")).toBeInTheDocument();
    expect(live).toHaveTextContent(/TLS/);
  });

  it("names the refusal reasons in the recent connections", async () => {
    addTcp();
    server.use(http.get("/api/v1/connection-logs", () => HttpResponse.json([
      { id: "l1", kind: "tcp_proxy", service_id: "svc_rdp01", tunnel_id: "", user_id: "", client_session_id: "", source_ip: "198.51.100.7", user_agent: "", started_at: "2026-10-24T08:00:00Z", ended_at: "2026-10-24T08:00:00Z", duration_ms: 0, bytes_in: 0, bytes_out: 0, status: "rejected", reason: "banned", protocol: "", sni: "" },
      { id: "l2", kind: "tcp_proxy", service_id: "svc_rdp01", tunnel_id: "", user_id: "", client_session_id: "", source_ip: "198.51.100.8", user_agent: "", started_at: "2026-10-24T08:01:00Z", ended_at: "2026-10-24T08:02:00Z", duration_ms: 60000, bytes_in: 5, bytes_out: 5, status: "closed_clean", reason: "idle_timeout", protocol: "ssh", sni: "" },
    ])));
    mount();
    const table = await screen.findByRole("table", { name: "Connections of this service" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(3));
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent(/refused/);
    expect(rows[1]).toHaveTextContent(/banned source/i);
    expect(rows[2]).toHaveTextContent(/idle timeout/i);
    expect(rows[2]).toHaveTextContent(/SSH/);
  });
});

describe("tcp service page: the password is asked for where the relay asks", () => {
  afterEach(() => vi.restoreAllMocks());

  function mountWithReauth() {
    db.clientLoginPasswordRequired = true;
    db.reauthConfirmed = false;
    return renderApp(
      <>
        <Routes><Route path="/services/:id" element={<ServiceDetail />} /></Routes>
        <ReauthDialog />
      </>,
      "/services/svc_rdp01",
    );
  }
  async function confirm() {
    const d = await screen.findByRole("dialog", { name: "Confirm your password" });
    // The prompt can open over another dialog: type once focus has moved into it, and check the text arrived.
    const input = within(d).getByLabelText("Your password");
    await waitFor(() => expect(input).toHaveFocus(), { timeout: 5_000 });
    await userEvent.type(input, "password123");
    await waitFor(() => expect(input).toHaveValue("password123"), { timeout: 5_000 });
    await userEvent.click(within(d).getByRole("button", { name: "Confirm" }));
  }

  it("host name", async () => {
    addTcp();
    mountWithReauth();
    const card = await region("Host name");
    await userEvent.type(await within(card).findByLabelText("Custom host name"), "rdp.example.org");
    await userEvent.click(within(card).getByRole("button", { name: "Save host name" }));
    await confirm();
    await waitFor(() => expect(db.tcpHostnames.svc_rdp01).toBe("rdp.example.org"), { timeout: 5_000 });
  });

  it("limits", async () => {
    addTcp();
    mountWithReauth();
    const card = await region("Limits");
    await userEvent.type(await within(card).findByLabelText("Bandwidth limit (kbit/s)"), "2000");
    await userEvent.click(within(card).getByRole("button", { name: "Save limits" }));
    await confirm();
    await waitFor(() => expect(db.tcpLimits.svc_rdp01?.rate_limit_kbps).toBe(2000), { timeout: 5_000 });
  });

  it("unban", async () => {
    addTcp();
    db.tcpBans.svc_rdp01 = [{ source: "198.51.100.7", until: new Date(Date.now() + 3600_000).toISOString(), reason: "auto", auto: true, created_at: "2026-10-24T08:00:00Z" }];
    mountWithReauth();
    const card = await region("Bans");
    await userEvent.click(await within(card).findByRole("button", { name: "Unban 198.51.100.7" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: /Unban/ })).getByRole("button", { name: "Unban" }));
    await confirm();
    await waitFor(() => expect(db.tcpBans.svc_rdp01).toHaveLength(0), { timeout: 5_000 });
  });

  it("countries", async () => {
    addTcp({ access_mode: "open" });
    mountWithReauth();
    const card = await region("Countries");
    await userEvent.type(await within(card).findByLabelText("Country code"), "ru");
    await userEvent.click(within(card).getByRole("button", { name: "Add country" }));
    await confirm();
    await waitFor(() => expect(db.tcpIpGeo.svc_rdp01?.block_countries).toEqual(["RU"]), { timeout: 5_000 });
  });
});

describe("tcp service page: small things", () => {
  afterEach(() => vi.restoreAllMocks());

  it("an unban of a ban that is gone closes the dialog and refreshes the list", async () => {
    addTcp();
    db.tcpBans.svc_rdp01 = [{ source: "198.51.100.7", until: new Date(Date.now() + 3600_000).toISOString(), reason: "auto", auto: true, created_at: "2026-10-24T08:00:00Z" }];
    mount();
    const card = await region("Bans");
    await userEvent.click(await within(card).findByRole("button", { name: "Unban 198.51.100.7" }));
    db.tcpBans.svc_rdp01 = []; // it ended meanwhile
    await userEvent.click(within(await screen.findByRole("dialog", { name: /Unban/ })).getByRole("button", { name: "Unban" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await within(card).findByText(/no source is banned/i)).toBeInTheDocument();
  });

  it("Enter does not add a country while Add is disabled", async () => {
    addTcp({ access_mode: "ip" });
    db.geoDatabase = false;
    const spy = vi.spyOn(globalThis, "fetch");
    mount();
    const card = await region("Countries");
    await waitFor(() => expect(within(card).getByRole("button", { name: "Add country" })).toBeDisabled());
    await userEvent.type(within(card).getByLabelText("Country code"), "de{Enter}");
    expect(sent(spy, "/ip-geo", "PUT")).toEqual([]);
  });

  it("names the file or the setting, and does not ask for a restart when the file is configured", async () => {
    addTcp({ access_mode: "ip" });
    db.geoDatabase = false;
    server.use(http.get("/api/v1/geo/status", () => HttpResponse.json({ enabled: false, configured: true, db_path: "/data/geo.mmdb", db_age_seconds: null, error: "bad file", rules_not_enforced: 0 })));
    mount();
    const card = await region("Countries");
    expect(await within(card).findByText(/\/data\/geo\.mmdb is not loaded \(bad file\)/)).toBeInTheDocument();
    expect(card).not.toHaveTextContent(/restart/i);
  });

  it("an open service without ipgeo:manage is told what the country rules need", async () => {
    addTcp({ access_mode: "open" });
    db.me = { id: db.me.id, email: "bob@acme.io", role: "operator", permissions: ["tunnels:read:own", "services:configure:own"] };
    mount();
    expect(await screen.findByText(/country rules needs the ipgeo:manage permission/i)).toBeInTheDocument();
  });
});
