import { test, expect, type Page } from "@playwright/test";

// Gateway depth (G10): system text and routing on a model, custom answer
// patterns and the judge on the Guardrails page, and a gateway key's system
// text — each set through the dashboard against the real relay and read back.
// Every object the spec creates is removed again at the end.

test.use({ storageState: "playwright-auth.json" });
test.describe.configure({ mode: "serial" });

const PROVIDER_A = "e2e-gd-a";
const PROVIDER_B = "e2e-gd-b";
const MODEL = "e2e-gd-duo";
const KEY_NAME = "e2e-gd-key";
const PATTERN_ID = "e2e-gd-ticket";

async function headers(page: Page) {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
  return { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
}

let savedGuardrails: Record<string, unknown> | null = null;
let keyId = "";

test.beforeAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: "playwright-auth.json" });
  const page = await context.newPage();
  const h = await headers(page);
  // Two tunnel providers, each on its own service: no traffic is sent.
  for (const slug of [PROVIDER_A, PROVIDER_B]) {
    const service = await page.request.post("/api/v1/services", {
      headers: h,
      data: { service_id: slug, title: slug, access_mode: "api_key" },
    });
    expect([201, 409]).toContain(service.status());
    const res = await page.request.post("/api/v1/ai/providers", {
      headers: h,
      data: { slug, name: slug, kind: "tunnel", service_id: slug },
    });
    expect([201, 409]).toContain(res.status());
  }
  const model = await page.request.post("/api/v1/ai/models", {
    headers: h,
    data: { name: MODEL, targets: [{ provider: PROVIDER_A, model: "gpt-a" }, { provider: PROVIDER_B, model: "gpt-b" }] },
  });
  expect([201, 409]).toContain(model.status());
  const key = await page.request.post("/api/v1/ai/keys", { headers: h, data: { name: KEY_NAME } });
  expect(key.status()).toBe(201);
  keyId = ((await key.json()) as { id: string }).id;
  const settings = await page.request.get("/api/v1/guardrails/settings");
  savedGuardrails = ((await settings.json()) as { global: Record<string, unknown> }).global;
  await context.close();
});

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: "playwright-auth.json" });
  const page = await context.newPage();
  const h = await headers(page);
  if (savedGuardrails) {
    await page.request.put("/api/v1/guardrails/settings", { headers: h, data: savedGuardrails });
  }
  if (keyId) await page.request.delete(`/api/v1/ai/keys/${keyId}`, { headers: h });
  await page.request.delete(`/api/v1/ai/models/${MODEL}`, { headers: h });
  for (const slug of [PROVIDER_A, PROVIDER_B]) {
    await page.request.delete(`/api/v1/ai/providers/${slug}`, { headers: h });
    await page.request.delete(`/api/v1/services/${slug}`, { headers: h });
  }
  await context.close();
});

async function openModelDialog(page: Page) {
  await page.goto("/gateway/models");
  const table = page.getByRole("table", { name: "Models" });
  await table.getByRole("button", { name: `More actions for ${MODEL}` }).click();
  await page.getByRole("menuitem", { name: "Edit" }).click();
  const dialog = page.getByRole("dialog", { name: `Edit ${MODEL}` });
  await expect(dialog).toBeVisible();
  return dialog;
}

test("a model's system text is saved and shown again when the dialog reopens", async ({ page }) => {
  let dialog = await openModelDialog(page);
  // Closed by default: the model has no text and the configured order.
  await dialog.getByRole("button", { name: "System text and routing" }).click();
  await dialog.getByLabel("System text").fill("Answer in German.");
  await expect(dialog.getByText("17 / 8192 bytes")).toBeVisible();
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await expect(dialog).toBeHidden();

  dialog = await openModelDialog(page);
  // Open by itself now that the model has a text.
  await expect(dialog.getByLabel("System text")).toHaveValue("Answer in German.");
  await dialog.getByRole("button", { name: "Cancel" }).click();
});

test("routing Fastest on a model with two targets shows on the Models list", async ({ page }) => {
  const dialog = await openModelDialog(page);
  await dialog.getByRole("radio", { name: "Fastest" }).check();
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await expect(dialog).toBeHidden();

  const row = page.getByRole("table", { name: "Models" }).getByRole("row").filter({ hasText: MODEL });
  await expect(row.getByText("Fastest", { exact: true })).toBeVisible();
  await expect(row.getByText("System text", { exact: true })).toBeVisible();
});

test("a custom answer pattern is added to the table and deleted again", async ({ page }) => {
  await page.goto("/gateway/guardrails");
  await page.getByRole("button", { name: /^Custom patterns/ }).click();
  await page.getByRole("button", { name: "Add pattern" }).click();
  const dialog = page.getByRole("dialog", { name: "New custom pattern" });
  await dialog.getByLabel("ID").fill(PATTERN_ID);
  await dialog.getByLabel("Pattern", { exact: true }).fill("TICKET-[0-9]{6}");
  await dialog.getByRole("combobox", { name: "Direction" }).click();
  await page.getByRole("option", { name: "Answers" }).click();
  await dialog.getByLabel("Description").fill("Internal ticket numbers");
  await dialog.getByRole("button", { name: "Add", exact: true }).click();
  await expect(dialog).toBeHidden();

  const table = page.getByRole("table", { name: "Custom patterns" });
  const row = table.getByRole("row").filter({ hasText: PATTERN_ID });
  await expect(row).toBeVisible();
  await expect(row.getByRole("cell", { name: "Answers" })).toBeVisible();

  // The confirmation is the dashboard's own dialog, not a native confirm().
  await row.getByRole("button", { name: `Delete pattern ${PATTERN_ID}` }).click();
  const confirm = page.getByRole("dialog", { name: "Delete pattern?" });
  await confirm.getByRole("button", { name: "Delete" }).click();
  await expect(table.getByRole("row").filter({ hasText: PATTERN_ID })).toHaveCount(0);
  await expect(table.getByText("No custom patterns yet.")).toBeVisible();
});

test("the judge's model is kept after a reload", async ({ page }) => {
  await page.goto("/gateway/guardrails");
  await page.getByRole("button", { name: /^Judge/ }).click();
  await page.getByRole("switch", { name: "Ask a model" }).click();
  await page.getByRole("combobox", { name: "Model" }).click();
  await page.getByRole("option", { name: MODEL, exact: true }).click();
  await page.getByRole("button", { name: "Save judge settings" }).click();
  await expect(page.getByText("Judge settings saved.")).toBeVisible();

  await page.reload();
  await page.getByRole("button", { name: /^Judge/ }).click();
  await expect(page.getByRole("combobox", { name: "Model" })).toHaveText(MODEL);
  await expect(page.getByRole("switch", { name: "Ask a model" })).toBeChecked();
});

test("a gateway key's system text is edited and shown again", async ({ page }) => {
  await page.goto("/gateway/keys");
  const table = page.getByRole("table", { name: "Gateway keys" });
  await table.getByRole("button", { name: `Edit ${KEY_NAME}` }).click();
  let dialog = page.getByRole("dialog", { name: `Edit ${KEY_NAME}` });
  await dialog.getByLabel("System text").fill("Use British spelling.");
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await expect(dialog).toBeHidden();

  await table.getByRole("button", { name: `Edit ${KEY_NAME}` }).click();
  dialog = page.getByRole("dialog", { name: `Edit ${KEY_NAME}` });
  await expect(dialog.getByLabel("System text")).toHaveValue("Use British spelling.");
  await dialog.getByRole("button", { name: "Cancel" }).click();
});
