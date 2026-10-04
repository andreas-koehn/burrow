import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import { AccessModePanel } from "@/components/AccessModePanel";

describe("AccessModePanel (v0.3.0)", () => {
  it("drops the v0.2.0 disabled gating — every mode is selectable", async () => {
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    expect(screen.queryByText(/needs http tunnels/i)).toBeNull();
    const radios = screen.getAllByRole("radio");
    // v0.4.0: mtls is the 4th mode (Task 6 mounts its dedicated panel).
    expect(radios).toHaveLength(4);
    for (const r of radios) expect(r).not.toHaveAttribute("aria-disabled", "true");
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    expect(screen.getByRole("radio", { name: /api key/i })).toHaveAttribute("aria-checked", "true");
  });

  it("api_key reveals the mono header field and mounts ApiKeysPanel", async () => {
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    const header = screen.getByLabelText(/api key header/i);
    expect(header).toHaveValue("Authorization");
    expect(header.className).toContain("mono");
    // ApiKeysPanel is mounted — the New key button is its identifying control.
    expect(
      await screen.findByRole("button", { name: "New key" }),
    ).toBeInTheDocument();
  });

  // P1-5: admins never see the services:configure permission hint (they
  // already have the permission); non-admins still get the explanation.
  it("hides the services:configure permission hint for admin users (P1-5)", async () => {
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    await screen.findByRole("button", { name: "New key" });
    expect(
      screen.queryByText("Managing keys needs the services:configure permission."),
    ).toBeNull();
  });

  it("shows the services:configure permission hint for non-admin users (P1-5)", async () => {
    db.me = { id: "u_user", email: "bob@acme.io", role: "user" };
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    expect(
      await screen.findByText("Managing keys needs the services:configure permission."),
    ).toBeInTheDocument();
  });

  it("burrow_login mounts the AccessPolicyEditor", async () => {
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /burrow login/i }));
    expect(
      await screen.findByText(/SSO: one Burrow login covers every protected service/i),
    ).toBeInTheDocument();
  });

  it("saving issues PUT /services/:id/access-mode and toasts success", async () => {
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    await userEvent.click(screen.getByRole("button", { name: /save changes/i }));
    expect((await screen.findAllByText(/access settings saved/i)).length).toBeGreaterThan(0);
  });

  it("a 409 on a tcp service surfaces the v0.4.0 contract message verbatim", async () => {
    renderApp(<AccessModePanel serviceId="svc_pg001" serviceName="postgres" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    await userEvent.click(screen.getByRole("button", { name: /save changes/i }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "api_key, burrow_login, and mtls require an http service",
      ),
    );
  });

  it("mtls mounts MtlsPanel and Save sends ca_pem to /services/:id/access-mode", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /mtls/i }));
    const pem = await screen.findByLabelText(/ca pem/i);
    await userEvent.type(pem, "-----BEGIN CERTIFICATE-----\nMIIB...=\n-----END CERTIFICATE-----");
    await userEvent.click(screen.getByRole("button", { name: /save changes/i }));
    await waitFor(() => {
      const put = fetchSpy.mock.calls.find(([url, init]) =>
        String(url).endsWith("/api/v1/services/svc_web01/access-mode")
        && (init as RequestInit | undefined)?.method === "PUT",
      );
      expect(put).toBeTruthy();
      const body = JSON.parse(String((put![1] as RequestInit).body));
      expect(body.access_mode).toBe("mtls");
      expect(typeof body.mtls_ca_pem).toBe("string");
      expect(body.mtls_ca_pem).toContain("BEGIN CERTIFICATE");
    });
    expect((await screen.findAllByText(/access settings saved/i)).length).toBeGreaterThan(0);
  });

  it("a 403 surfaces a friendly permission message", async () => {
    db.me = { id: "bur_usr_other", email: "x@y.io", role: "user" };
    renderApp(<AccessModePanel serviceId="svc_web01" serviceName="web" mode="open" />);
    await userEvent.click(screen.getByRole("radio", { name: /api key/i }));
    await userEvent.click(screen.getByRole("button", { name: /save changes/i }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "You don't have permission to configure this service.",
      ),
    );
  });
});
