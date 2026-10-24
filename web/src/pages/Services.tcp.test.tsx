import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { db, type ServiceRow } from "@/mocks/db";
import Services from "@/pages/Services";
import { ReauthDialog } from "@/components/ReauthDialog";

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

  it("a private service's command uses its slug, else its name", async () => {
    addTcp({ slug: "t3f9a1c07be" });
    addTcp({ id: "svc_rdp02", name: "rdp-two", slug: "" });
    renderApp(<Services />, "/services");
    const table = await screen.findByRole("table", { name: "Services" });
    const withSlug = within(table).getByText("rdp").closest("tr")!;
    expect(within(withSlug).getByText("burrow forward t3f9a1c07be 13389")).toBeInTheDocument();
    const without = within(table).getByText("rdp-two").closest("tr")!;
    expect(within(without).getByText("burrow forward rdp-two 13389")).toBeInTheDocument();
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

describe("New service: a tcp service", () => {
  afterEach(() => vi.restoreAllMocks());

  async function openTcp() {
    renderApp(<Services />, "/services");
    await userEvent.click(await screen.findByRole("button", { name: "New service" }));
    const dialog = await screen.findByRole("dialog", { name: "New service" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Create a TCP service instead" }));
    return screen.findByRole("dialog", { name: "New TCP service" });
  }
  const posts = (spy: { mock: { calls: unknown[][] } }) =>
    spy.mock.calls
      .filter(([u, i]) => String(u).endsWith("/api/v1/services") && (i as RequestInit | undefined)?.method === "POST")
      .map(([, i]) => JSON.parse(String((i as RequestInit).body)));

  it("offers the other kind as a button in each dialog, and the dialog opens on no choice", async () => {
    renderApp(<Services />, "/services");
    await userEvent.click(await screen.findByRole("button", { name: "New service" }));
    const web = await screen.findByRole("dialog", { name: "New service" });
    expect(within(web).queryByRole("radiogroup")).toBeNull();
    await userEvent.click(within(web).getByRole("button", { name: "Create a TCP service instead" }));
    const tcp = await screen.findByRole("dialog", { name: "New TCP service" });
    await userEvent.click(within(tcp).getByRole("button", { name: "Create a web service instead" }));
    expect(await screen.findByRole("dialog", { name: "New service" })).toBeInTheDocument();
  });

  it("creates a private service by default and then shows the command to run", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    const dialog = await openTcp();
    expect(within(dialog).getByRole("radio", { name: "Private" })).toBeChecked();
    expect(within(dialog).queryByLabelText("Public port")).toBeNull();
    await userEvent.type(within(dialog).getByLabelText("Name"), "rdp-office");
    await userEvent.type(within(dialog).getByLabelText("Port of the local service"), "3389");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posts(spy)).toEqual([{ type: "tcp", name: "rdp-office", access_mode: "private" }]));
    expect(await within(dialog).findByText("burrow tcp 3389 --name rdp-office")).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/type: tcp/);
    expect(dialog).toHaveTextContent(/local: 127\.0\.0\.1:3389/);
    expect(db.services.some((s) => s.name === "rdp-office" && s.type === "tcp" && s.access_mode === "private")).toBe(true);
    expect(within(dialog).getByRole("link", { name: "Open the service" })).toHaveAttribute("href", expect.stringMatching(/^\/services\/svc_/));
  });

  it("takes a mode and a fixed public port, and warns for open", async () => {
    const spy = vi.spyOn(globalThis, "fetch");
    const dialog = await openTcp();
    await userEvent.type(within(dialog).getByLabelText("Name"), "ssh-box");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Open" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent(/anyone on the internet/i);
    await userEvent.click(within(dialog).getByRole("radio", { name: "IP allowlist" }));
    await userEvent.type(within(dialog).getByLabelText("Public port"), "9042");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posts(spy)).toEqual([{ type: "tcp", name: "ssh-box", access_mode: "ip", tcp_port: 9042 }]));
    expect(await within(dialog).findByText(/burrow tcp <port> --name ssh-box/)).toBeInTheDocument();
  });

  it("will not send a name with a space, nor create without a name", async () => {
    const dialog = await openTcp();
    const create = within(dialog).getByRole("button", { name: "Create" });
    expect(create).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText("Name"), "my service");
    expect(create).toBeDisabled();
  });

  it("shows the relay's refusal at the field", async () => {
    server.use(http.post("/api/v1/services", () => HttpResponse.json({ error: "tcp_port is held by another service" }, { status: 409 })));
    const dialog = await openTcp();
    await userEvent.type(within(dialog).getByLabelText("Name"), "ssh-box");
    await userEvent.click(within(dialog).getByRole("radio", { name: "IP allowlist" }));
    await userEvent.type(within(dialog).getByLabelText("Public port"), "9042");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByText(/held by another service/)).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Public port")).toHaveValue(9042);
  });

  it("says a name that exists already", async () => {
    addTcp({ name: "rdp" });
    const dialog = await openTcp();
    await userEvent.type(within(dialog).getByLabelText("Name"), "rdp");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByText(/already exists/)).toBeInTheDocument();
  });

  it("asks for the password before it opens a port, and not for a private service", async () => {
    db.clientLoginPasswordRequired = true;
    db.reauthConfirmed = false;
    renderApp(<><Services /><ReauthDialog /></>, "/services");
    await userEvent.click(await screen.findByRole("button", { name: "New service" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "New service" })).getByRole("button", { name: "Create a TCP service instead" }));
    const dialog = await screen.findByRole("dialog", { name: "New TCP service" });
    // Private: no prompt.
    await userEvent.type(within(dialog).getByLabelText("Name"), "quiet-box");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await within(dialog).findByText(/burrow tcp/);
    expect(db.services.some((s) => s.name === "quiet-box")).toBe(true);
    await userEvent.click(within(dialog).getByRole("button", { name: "Done" }));

    await userEvent.click(await screen.findByRole("button", { name: "New service" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "New service" })).getByRole("button", { name: "Create a TCP service instead" }));
    const second = await screen.findByRole("dialog", { name: "New TCP service" });
    await userEvent.type(within(second).getByLabelText("Name"), "ssh-box");
    await userEvent.click(within(second).getByRole("radio", { name: "Open" }));
    await userEvent.click(within(second).getByRole("button", { name: "Create" }));
    const prompt = await screen.findByRole("dialog", { name: "Confirm your password" });
    expect(db.services.some((s) => s.name === "ssh-box")).toBe(false);
    await userEvent.type(within(prompt).getByLabelText("Your password"), "password123");
    await userEvent.click(within(prompt).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(db.services.some((s) => s.name === "ssh-box" && s.access_mode === "open")).toBe(true));
  });
});
