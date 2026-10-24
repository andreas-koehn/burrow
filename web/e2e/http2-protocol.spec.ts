import { test, expect } from "@playwright/test";
import { answerReauth, guardedRequest } from "./reauth";

// HTTP/2 and gRPC on an http service, against the REAL built burrowd: the Protocol card writes
// http-settings (read back through the API), and the Host names card shows what the relay says
// about an automatic certificate. The e2e relay runs without ACME, so it refuses `auto` with its
// own text, which the dialog must show. No client is needed: the cards write the stored service.

const NAME = `pw-h2-${Date.now()}`;

test.describe("Protocol and Host names cards", () => {
  test.use({ storageState: "playwright-auth.json" });

  test.beforeAll(async ({ browser }) => {
    const page = await (await browser.newContext({ storageState: "playwright-auth.json" })).newPage();
    await page.goto("/");
    const created = await guardedRequest(page, "POST", "/api/v1/services", { service_id: NAME, title: NAME, access_mode: "api_key" });
    expect([201, 409]).toContain(created.status());
    await page.context().close();
  });

  test("the Protocol card saves the upstream protocol, TLS fields and the gRPC cap", async ({ page }) => {
    await page.goto(`/services/${NAME}`);
    const card = page.getByRole("region", { name: "Protocol", exact: true });
    await expect(card).toBeVisible();
    await expect(card.getByRole("combobox", { name: "Upstream protocol" })).toContainText("HTTP/1.1");
    // The notices the spec asks for.
    await expect(card.getByText(/not recorded by the inspector/)).toBeVisible();
    await expect(card.getByText(/refused.*AI controls/)).toBeVisible();
    // api_key mode: nothing about burrow_login.
    await expect(card.getByText(/burrow_login/)).toHaveCount(0);

    // h2c: no TLS fields.
    await card.getByRole("combobox", { name: "Upstream protocol" }).click();
    await page.getByRole("option", { name: /^h2c/ }).click();
    await expect(card.getByLabel("Server name")).toHaveCount(0);
    await card.getByLabel("Maximum gRPC call duration (seconds)").fill("600");
    await card.getByRole("button", { name: "Save" }).click();
    await expect.poll(async () => {
      const s = (await (await page.request.get(`/api/v1/services/${NAME}/http-settings`)).json()) as { upstream_protocol: string; grpc_max_duration_s: number };
      return `${s.upstream_protocol}/${s.grpc_max_duration_s}`;
    }).toBe("h2c/600");

    // h2: TLS fields appear; a CA that is not a certificate is refused with the relay's text.
    await card.getByRole("combobox", { name: "Upstream protocol" }).click();
    await page.getByRole("option", { name: /^h2 \(/ }).click();
    await expect(card.getByLabel("Verify the upstream certificate")).toBeVisible();
    await card.getByLabel("Server name").fill("app.internal");
    await card.getByLabel("CA certificate (PEM)").fill("not a certificate");
    await card.getByRole("button", { name: "Save" }).click();
    await expect(card.getByRole("alert")).toContainText("upstream_tls_ca_pem must be PEM");

    // Without the CA the settings are stored; verification is a choice, off by default and explained.
    await card.getByLabel("CA certificate (PEM)").fill("");
    await expect(card.getByText(/not authenticated/)).toBeVisible();
    await card.getByRole("switch", { name: "Verify the upstream certificate" }).click();
    await card.getByRole("button", { name: "Save" }).click();
    await expect.poll(async () => {
      const s = (await (await page.request.get(`/api/v1/services/${NAME}/http-settings`)).json()) as { upstream_protocol: string; upstream_tls_verify: boolean; upstream_tls_server_name: string };
      return `${s.upstream_protocol}/${s.upstream_tls_verify}/${s.upstream_tls_server_name}`;
    }).toBe("h2/true/app.internal");
    // The card reads the stored values back, and shows what is in effect.
    await page.reload();
    await expect(card.getByLabel("Server name")).toHaveValue("app.internal");
    await expect(card.getByText("In effect")).toBeVisible();
  });

  test("the Host names card adds an automatic domain, shows its status and gRPC address, and removes it", async ({ page }) => {
    await page.goto(`/services/${NAME}`);
    const card = page.getByRole("region", { name: "Host names", exact: true });
    await expect(card.getByText("No custom domains yet.")).toBeVisible();
    await card.getByRole("button", { name: "New domain" }).click();
    const dialog = page.getByRole("dialog", { name: "Add custom domain" });
    // The seeded administrator holds services:configure:any, so both sources are offered.
    await expect(dialog.getByRole("radio", { name: /Upload a certificate/ })).toHaveAttribute("aria-checked", "true");
    await dialog.getByLabel("Hostname").fill("grpc.example.org");
    await dialog.getByRole("radio", { name: /Automatic/ }).click();
    await expect(dialog.getByLabel("Certificate (PEM)")).toHaveCount(0);
    await dialog.getByRole("button", { name: "Add", exact: true }).click();
    // The password may be asked for first (no services domain): answer it once.
    await answerReauth(page, card.getByText("grpc.example.org", { exact: true }));
    await expect(dialog).toBeHidden();

    // The e2e relay's dashboard is a plain listener (a proxy would end TLS): no certificate is needed.
    const row = card.getByRole("row", { name: /grpc\.example\.org/ });
    await expect(row).toContainText("Automatic");
    await expect(row).toContainText("Handled by the proxy");
    await expect(row.getByText("grpc.example.org:443")).toBeVisible();
    await expect(row.getByRole("button", { name: "Copy gRPC address grpc.example.org:443" })).toBeVisible();

    // The relay refuses a second service the same name; its text is shown in the dialog.
    await card.getByRole("button", { name: "New domain" }).click();
    await dialog.getByLabel("Hostname").fill("grpc.example.org");
    await dialog.getByRole("radio", { name: /Automatic/ }).click();
    await dialog.getByRole("button", { name: "Add", exact: true }).click();
    await expect(dialog.getByRole("alert")).toContainText(/already bound/);
    await dialog.getByRole("button", { name: "Cancel" }).click();

    await row.getByRole("button", { name: "Delete domain grpc.example.org" }).click();
    await page.getByRole("dialog", { name: "Remove custom domain" }).getByRole("button", { name: "Remove" }).click();
    await expect(card.getByText("No custom domains yet.")).toBeVisible();
  });
});
