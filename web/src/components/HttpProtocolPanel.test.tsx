import { describe, it, expect, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { resetDb, db } from "@/mocks/db";
import { HttpProtocolPanel } from "./HttpProtocolPanel";

const SVC = "svc_ai001";
function mount(accessMode = "api_key", serviceId = SVC) {
  return renderApp(<HttpProtocolPanel serviceId={serviceId} accessMode={accessMode as never} />);
}
const card = async () => screen.findByRole("region", { name: "Protocol" });

// The PUT bodies the relay received.
function captureBodies() {
  const bodies: Record<string, unknown>[] = [];
  server.use(http.put(`/api/v1/services/${SVC}/http-settings`, async ({ request }) => {
    const b = (await request.clone().json()) as Record<string, unknown>;
    bodies.push(b);
    const row = db.httpSettings[SVC] ?? { upstream_protocol: "http1", upstream_tls_verify: false, upstream_tls_server_name: "", upstream_tls_ca_pem: "", grpc_max_duration_s: 0, client_upstream_protocol: "" };
    db.httpSettings[SVC] = { ...row, ...b } as never;
    return HttpResponse.json({
      ...db.httpSettings[SVC], upstream_tls_ca_sha256: "", effective_upstream_protocol: db.httpSettings[SVC]!.upstream_protocol,
    });
  }));
  return bodies;
}

beforeEach(() => resetDb());

describe("HttpProtocolPanel", () => {
  it("shows http1 as the upstream protocol by default, with no TLS fields", async () => {
    mount();
    const c = await card();
    expect(await within(c).findByRole("combobox", { name: "Upstream protocol" })).toHaveTextContent("HTTP/1.1");
    expect(within(c).queryByLabelText("Verify the upstream certificate")).toBeNull();
    expect(within(c).queryByLabelText("Server name")).toBeNull();
  });

  it("offers http1, https, h2c and h2 and saves the choice", async () => {
    const bodies = captureBodies();
    mount();
    const c = await card();
    await userEvent.click(await within(c).findByRole("combobox", { name: "Upstream protocol" }));
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toHaveLength(4);
    expect(options[0]).toMatch(/HTTP\/1\.1/);
    await userEvent.click(screen.getByRole("option", { name: /h2c/ }));
    await userEvent.click(within(c).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ upstream_protocol: "h2c", upstream_tls_verify: false, grpc_max_duration_s: 0 });
  });

  it("shows TLS fields for https and h2 only", async () => {
    db.httpSettings[SVC] = { upstream_protocol: "h2", upstream_tls_verify: true, upstream_tls_server_name: "app.internal", upstream_tls_ca_pem: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----", grpc_max_duration_s: 600, client_upstream_protocol: "" };
    mount();
    const c = await card();
    expect(await within(c).findByLabelText("Verify the upstream certificate")).toBeChecked();
    expect(within(c).getByLabelText("Server name")).toHaveValue("app.internal");
    expect(within(c).getByLabelText("Maximum gRPC call duration (seconds)")).toHaveValue(600);
  });

  it("explains verify off: the connection is encrypted but not authenticated", async () => {
    db.httpSettings[SVC] = { upstream_protocol: "https", upstream_tls_verify: false, upstream_tls_server_name: "", upstream_tls_ca_pem: "", grpc_max_duration_s: 0, client_upstream_protocol: "" };
    mount();
    const c = await card();
    expect(await within(c).findByText(/not authenticated/i)).toBeInTheDocument();
  });

  it("shows only the fingerprint of the stored CA, never the PEM, and can replace or clear it", async () => {
    db.httpSettings[SVC] = { upstream_protocol: "https", upstream_tls_verify: true, upstream_tls_server_name: "", upstream_tls_ca_pem: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----", grpc_max_duration_s: 0, client_upstream_protocol: "" };
    const bodies = captureBodies();
    mount();
    const c = await card();
    expect(await within(c).findByText(/sha256_/)).toBeInTheDocument();
    expect(c.textContent).not.toContain("BEGIN CERTIFICATE");
    await userEvent.click(within(c).getByRole("button", { name: "Remove CA" }));
    await userEvent.click(within(c).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]!.upstream_tls_ca_pem).toBe("");
  });

  it("sends a CA the reader pastes, and shows an error the relay gives", async () => {
    db.httpSettings[SVC] = { upstream_protocol: "https", upstream_tls_verify: true, upstream_tls_server_name: "", upstream_tls_ca_pem: "", grpc_max_duration_s: 0, client_upstream_protocol: "" };
    const bodies = captureBodies();
    mount();
    const c = await card();
    const ta = await within(c).findByLabelText("CA certificate (PEM)");
    await userEvent.click(ta);
    await userEvent.paste("-----BEGIN CERTIFICATE-----\nxyz\n-----END CERTIFICATE-----");
    await userEvent.click(within(c).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(String(bodies[0]!.upstream_tls_ca_pem)).toContain("BEGIN CERTIFICATE");
  });

  it("shows a refusal of the relay next to the form", async () => {
    server.use(http.put(`/api/v1/services/${SVC}/http-settings`, () =>
      HttpResponse.json({ error: "grpc_max_duration_s must be 0 (no cap) or 1 to 86400" }, { status: 400 })));
    mount();
    const c = await card();
    const n = await within(c).findByLabelText("Maximum gRPC call duration (seconds)");
    await userEvent.clear(n);
    await userEvent.type(n, "5");
    await userEvent.click(within(c).getByRole("button", { name: "Save" }));
    expect(await within(c).findByRole("alert")).toHaveTextContent("grpc_max_duration_s must be 0 (no cap) or 1 to 86400");
  });

  it("shows the protocol a client names and the one in effect, read only", async () => {
    db.httpSettings[SVC] = { upstream_protocol: "http1", upstream_tls_verify: false, upstream_tls_server_name: "", upstream_tls_ca_pem: "", grpc_max_duration_s: 0, client_upstream_protocol: "h2c" };
    mount();
    const c = await card();
    expect(await within(c).findByText("Set by the client")).toBeInTheDocument();
    expect(within(c).getByText("In effect")).toBeInTheDocument();
    const facts = c.querySelector("dl")!;
    expect(facts).toHaveTextContent(/Set by the client\s*h2c/);
    expect(facts).toHaveTextContent(/In effect\s*h2c/);
    expect(within(c).getByText(/overrides the choice/i)).toBeInTheDocument();
  });

  it("says that burrow_login is for browsers, only in that mode", async () => {
    mount("burrow_login");
    const c = await card();
    expect(await within(c).findByText(/burrow_login.*browsers/i)).toBeInTheDocument();
  });

  it("says nothing about burrow_login in the api_key mode", async () => {
    mount("api_key");
    const c = await card();
    await within(c).findByRole("combobox", { name: "Upstream protocol" });
    expect(within(c).queryByText(/burrow_login/)).toBeNull();
  });

  it("says that gRPC calls are not recorded by the inspector and refused with AI controls on", async () => {
    mount();
    const c = await card();
    await within(c).findByRole("combobox", { name: "Upstream protocol" });
    expect(within(c).getByText(/not recorded by the inspector/i)).toBeInTheDocument();
    expect(within(c).getByText(/refused.*AI controls|AI controls.*refused/i)).toBeInTheDocument();
  });

  it("tells that the relay does not accept h2c from visitors when it is off", async () => {
    db.ingress = { h2c: false, https_passthrough: false, passthrough_listen: "", acme: false };
    mount();
    const c = await card();
    expect(await within(c).findByText(/BURROW_HTTP_H2C/)).toBeInTheDocument();
  });

  it("is read only for someone without services:configure", async () => {
    db.me = { ...db.me, role: "user", permissions: ["tunnels:read:own"] };
    mount();
    // The relay answers 403: the card says so and offers no controls.
    expect(await screen.findByText(/need.*services:configure/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("an owner with services:configure:own can save", async () => {
    db.me = { ...db.me, role: "user", permissions: ["services:configure:own"] };
    mount();
    const c = await card();
    expect(await within(c).findByRole("button", { name: "Save" })).toBeEnabled();
  });
});
