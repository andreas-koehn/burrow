import { test, expect } from "@playwright/test";

// v0.4.0: /audit — hash-chained audit log surface (spec Part G.2). Asserts
// the page mounts, the events table renders, and "Verify chain" produces
// the success role=status banner.

test.use({ storageState: "playwright-auth.json" });

test("v0.4.0: audit log table renders + Verify chain succeeds", async ({ page }) => {
  await page.goto("/settings/audit");
  await expect(page).toHaveURL(/\/settings\/audit$/);
  await expect(page.getByRole("heading", { name: "Audit log" })).toBeVisible();

  // Events table renders with the documented header columns.
  const table = page.getByRole("table", { name: "Audit events" });
  await expect(table).toBeVisible();
  await expect(table.getByRole("columnheader", { name: "Action" })).toBeVisible();
  await expect(table.getByRole("columnheader", { name: "Actor" })).toBeVisible();

  // Verify chain → POST /audit/verify → role=status banner with "Chain valid".
  await page.getByRole("button", { name: "Verify chain" }).click();
  await expect(page.getByRole("status")).toContainText(/Chain valid from/);
});

test("v0.4.0: audit log API — /events list + /verify ok envelope", async ({ page }) => {
  const list = await page.request.get("/api/v1/audit/events?limit=10");
  expect(list.status()).toBe(200);
  const events = await list.json();
  expect(Array.isArray(events)).toBe(true);

  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  expect(csrf).not.toBe("");
  const verify = await page.request.post("/api/v1/audit/verify", {
    headers: { "X-CSRF-Token": csrf, "Content-Type": "application/json" },
    data: {},
  });
  expect(verify.status()).toBe(200);
  const body = (await verify.json()) as Record<string, unknown>;
  expect(typeof body.ok).toBe("boolean");
});
