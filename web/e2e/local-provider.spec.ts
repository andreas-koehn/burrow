import { test, expect, type Page } from "@playwright/test";

// A local model server, from nothing to a provider that waits for its client:
// the whole way through the AI Gateway workspace, without a detour through
// Services. Runs against the real burrowd as the seeded admin. No client
// serves anything in this stack, so the provider stays "Never connected":
// that it turns Online is covered by cmd/server/e2e_local_provider_test.go.

test.use({ storageState: "playwright-auth.json" });

const sidebar = (page: Page) => page.locator(".sidebar");
const workspaceTab = (page: Page, name: string) =>
  sidebar(page).getByRole("navigation", { name: "Workspace" }).getByRole("link", { name, exact: true });

async function expectGatewaySidebar(page: Page) {
  await expect(workspaceTab(page, "AI Gateway")).toHaveAttribute("aria-current", "true");
  await expect(page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Providers" })).toBeVisible();
}

async function csrfHeaders(page: Page) {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
  return { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
}

/**
 * Removes the provider this spec makes. Its service stays (the relay has no
 * route that deletes one), so the first test needs the fresh database every
 * Playwright run starts with.
 */
async function removeOllama(page: Page) {
  const headers = await csrfHeaders(page);
  await page.request.delete("/api/v1/ai/providers/ollama", { headers }).catch(() => {});
}

test("a local model server: New provider, the command, and a provider that waits for its client", async ({ page }) => {
  await page.goto("/gateway");
  await removeOllama(page);

  // 1. AI Gateway → Providers → New provider: the machine is the first choice.
  await page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Providers" }).click();
  await expect(page).toHaveURL(/\/gateway\/providers$/);
  await page.getByRole("button", { name: "New provider" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New provider" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("radio", { name: "A model server on one of my machines" })).toBeChecked();
  // Nothing is an error before anything was typed.
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  await expectGatewaySidebar(page);

  await dialog.getByLabel("Name").fill("Ollama");
  await dialog.getByRole("button", { name: "Continue" }).click();

  await expect(dialog.getByRole("heading", { name: "Run this on the machine" })).toBeVisible({ timeout: 10_000 });
  await expect(dialog.getByRole("group", { name: "Run command" })).toHaveText("burrow http 11434 --name ollama");
  await expect(dialog.locator("[role=status]").filter({ hasText: "Waiting for the client…" })).toBeVisible();
  // 2. Still in the AI Gateway: the flow never left for Services.
  await expectGatewaySidebar(page);
  await expect(page).toHaveURL(/\/gateway\/providers/);

  // 3. Done → the provider's page, with the same command and the same status words.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).not.toBeVisible();
  await expect(page).toHaveURL(/\/gateway\/providers\/ollama$/, { timeout: 10_000 });
  await expectGatewaySidebar(page);
  const client = page.getByRole("region", { name: "Client" });
  await expect(client).toBeVisible({ timeout: 10_000 });
  await expect(client.locator("[role=status]").first()).toContainText("Never connected");
  await expect(client.getByRole("group", { name: "Run command" })).toHaveText("burrow http 11434 --name ollama");

  // The list says it with the same words.
  await page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Providers" }).click();
  const row = page.getByRole("table", { name: "Providers" }).getByRole("row").filter({ hasText: "Ollama" });
  await expect(row).toContainText("Never connected");

  await removeOllama(page);
});

test("Clients keeps the sidebar of the workspace it was opened from", async ({ page }) => {
  // 4. From the AI Gateway …
  await page.goto("/gateway/providers");
  await page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: /^Clients(,|$)/ }).click();
  await expect(page).toHaveURL(/\/clients$/);
  await expect(page.getByRole("heading", { name: "Clients", level: 1 })).toBeVisible();
  await expectGatewaySidebar(page);

  // … and from Services.
  await page.goto("/services");
  await page.getByRole("navigation", { name: "Services" }).getByRole("link", { name: /^Clients(,|$)/ }).click();
  await expect(page).toHaveURL(/\/clients$/);
  await expect(workspaceTab(page, "Services")).toHaveAttribute("aria-current", "true");
  await expect(page.getByRole("navigation", { name: "Services" }).getByRole("link", { name: "Traffic" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "AI Gateway" })).toHaveCount(0);
});

test("/services?new=ai lands on the guided flow", async ({ page }) => {
  // 5. The old way in.
  await page.goto("/services?new=ai");
  await expect(page).toHaveURL(/\/gateway\/providers\?new=local$/);
  const dialog = page.getByRole("dialog", { name: "New provider" });
  await expect(dialog.getByRole("radio", { name: "A model server on one of my machines" })).toBeChecked();
  await expectGatewaySidebar(page);
});

test("Requests opens without a provider preselected", async ({ page }) => {
  // Without any provider the page has nothing to choose from: one that is there for sure.
  await page.goto("/gateway");
  const headers = await csrfHeaders(page);
  const service = await page.request.post("/api/v1/services", {
    headers,
    data: { service_id: "e2e-requests-svc", title: "e2e-requests-svc", access_mode: "api_key" },
  });
  expect([201, 409]).toContain(service.status());
  const provider = await page.request.post("/api/v1/ai/providers", {
    headers,
    data: { slug: "e2e-requests", name: "E2E requests", kind: "tunnel", service_id: "e2e-requests-svc", gateway_only: true },
  });
  expect([201, 409]).toContain(provider.status());

  // 6. All providers, and nothing of that in the address.
  await page.getByRole("navigation", { name: "AI Gateway" }).getByRole("link", { name: "Requests" }).click();
  await expect(page).toHaveURL(/\/gateway\/requests$/);
  await expect(page.getByRole("heading", { name: "Requests", level: 1 })).toBeVisible();
  expect(new URL(page.url()).search).toBe("");
  await expect(page.getByLabel("Provider", { exact: true })).toHaveText("All");
  // The provider is one of the choices, not the one chosen.
  await page.getByLabel("Provider", { exact: true }).click();
  await expect(page.getByRole("option", { name: "E2E requests" })).toBeVisible();
  await expect(page.getByRole("option", { name: "All" })).toHaveAttribute("aria-selected", "true");
});

test("New model opens without an error, and says what is missing as fields are filled", async ({ page }) => {
  // 7. The hint beside the disabled button follows the form.
  await page.goto("/gateway/models?new=1");
  const dialog = page.getByRole("dialog", { name: "New model" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  await expect(dialog.locator("[aria-invalid=true]")).toHaveCount(0);

  const create = dialog.getByRole("button", { name: "Create model" });
  await expect(create).toBeDisabled();
  await expect(dialog.getByText("Enter a name.")).toBeVisible();

  await dialog.getByLabel("Model name", { exact: true }).fill("e2e-hint-model");
  await expect(dialog.getByText("Enter a name.")).toHaveCount(0);
  await expect(dialog.getByText("Choose a provider and a model for at least one format.")).toBeVisible();
  await expect(create).toBeDisabled();
  // Typing a name is still no error.
  await expect(dialog.getByRole("alert")).toHaveCount(0);
});
