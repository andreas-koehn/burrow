import { test, expect, type Page } from "@playwright/test";
import { ADMIN_PASSWORD, guardedRequest } from "./reauth";

// The settings a tcp service gained in "TCP complete": the service is made in
// the New service dialog before any client exists, then its Connection
// settings, Limits and Bans cards are used. No client is needed: every card
// writes the stored service, which the API reads back.

const NAME = "e2e-tcp-complete";

test.describe("tcp service settings", () => {
  test.use({ storageState: "playwright-auth.json" });

  async function cleanup(page: Page) {
    const services = (await (await page.request.get("/api/v1/services")).json()) as { id: string; name: string; type: string }[];
    for (const s of services.filter((x) => x.name === NAME && x.type === "tcp")) {
      await guardedRequest(page, "DELETE", `/api/v1/services/${s.id}`);
    }
  }

  // Waits until check() holds, answering the "Confirm your password" dialog
  // if a guarded save opens it (the session's window may already be open).
  async function settle(page: Page, check: () => Promise<boolean>) {
    const dialog = page.getByRole("dialog", { name: "Confirm your password" });
    await expect.poll(async () => {
      if (await dialog.isVisible()) {
        await dialog.getByLabel("Your password").fill(ADMIN_PASSWORD);
        await dialog.getByRole("button", { name: "Confirm" }).click();
        await expect(dialog).toBeHidden();
      }
      return check();
    }, { timeout: 20_000 }).toBe(true);
  }

  test("create in the dialog, then connection settings, limits, a ban and unban", async ({ page }) => {
    await page.goto("/services");
    await cleanup(page);
    await page.reload();

    // Create: private, so no recent confirmation is asked for.
    await page.getByRole("button", { name: "New service" }).click();
    await page.getByRole("button", { name: "Create a TCP service instead" }).click();
    const dialog = page.getByRole("dialog", { name: "New TCP service" });
    await dialog.getByLabel("Name").fill(NAME);
    await expect(dialog.getByRole("radio", { name: /^Private/ })).toHaveAttribute("aria-checked", "true");
    await dialog.getByLabel("Port of the local service").fill("3389");
    await dialog.getByRole("button", { name: "Create", exact: true }).click();
    await expect(dialog.getByText(`burrow tcp 3389 --name ${NAME}`)).toBeVisible();
    await dialog.getByRole("link", { name: "Open the service" }).click();
    await expect(page).toHaveURL(/\/services\/[0-9a-f-]+$/);
    const id = decodeURIComponent(page.url().split("/services/")[1]);
    const api = async <T,>(p: string) => (await (await page.request.get(`/api/v1/services/${id}${p}`)).json()) as T;
    expect((await api<{ access_mode: string; type: string }>("")).access_mode).toBe("private");

    // Connection settings: PROXY v2 and a 30-minute idle timeout.
    const settings = page.getByRole("region", { name: "Connection settings", exact: true });
    await settings.getByRole("switch", { name: "PROXY protocol v2" }).click();
    await settings.getByLabel("Idle timeout (minutes)").fill("30");
    await settings.getByRole("button", { name: "Save settings" }).click();
    await settle(page, async () => {
      const s = await api<{ proxy_protocol: string; idle_timeout_s: number }>("/tcp-settings");
      return s.proxy_protocol === "v2" && s.idle_timeout_s === 1800;
    });

    // Limits: 1000 kbit/s and automatic bans after 5 short connections.
    const limits = page.getByRole("region", { name: "Limits", exact: true });
    await limits.getByLabel("Bandwidth limit (kbit/s)").fill("1000");
    await limits.getByRole("switch", { name: "Automatic bans" }).click();
    await limits.getByLabel("Connections", { exact: true }).fill("5");
    await limits.getByRole("button", { name: "Save limits" }).click();
    await settle(page, async () => {
      const l = await api<{ rate_limit_kbps: number; autoban: { enabled: boolean; count: number } }>("/tcp-limits");
      return l.rate_limit_kbps === 1000 && l.autoban.enabled && l.autoban.count === 5;
    });

    // A ban by hand shows in the list; Unban asks first and lifts it.
    const bans = page.getByRole("region", { name: "Bans", exact: true });
    await bans.getByLabel("Source address").fill("203.0.113.9");
    await bans.getByRole("button", { name: "Ban source" }).click();
    const table = bans.getByRole("table", { name: "Banned sources" });
    await expect(table.getByRole("row").filter({ hasText: "203.0.113.9" })).toBeVisible();
    await table.getByRole("button", { name: "Unban 203.0.113.9" }).click();
    await page.getByRole("dialog", { name: /^Unban/ }).getByRole("button", { name: "Unban" }).click();
    await settle(page, async () => (await api<unknown[]>("/tcp-bans")).length === 0);
    await expect(bans.getByText("No source is banned.")).toBeVisible();

    await cleanup(page);
  });
});
