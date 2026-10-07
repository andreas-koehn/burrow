// test-only — never deploy this shape.
//
// Covers the /users list UI controls that the feature suite did not yet
// exercise: the email search box, the Role filter Select, and the pagination
// controls. See web/src/pages/Users.tsx. Two throwaway users (one "user",
// one "admin") are created via the API and removed in finally.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

test.use({ storageState: AUTH_STORAGE_PATH });

test("46-users-search-filter: email search + Role filter select; pagination controls", async ({ page, request }) => {
  const stamp = Date.now();
  const userEmail = `searchA-${stamp}@e2e.local`;
  const adminEmail = `searchB-${stamp}@e2e.local`;

  const mkUser = async (email: string, role: "user" | "admin") => {
    const res = await request.post("/api/v1/users", {
      headers: adminHeaders(),
      data: { email, password: "init-pass-123", role },
    });
    expect(res.status(), `create ${role} ${email}`).toBe(201);
    return (await res.json()).id as string;
  };

  let userId = "";
  let adminId = "";
  try {
    userId = await mkUser(userEmail, "user");
    adminId = await mkUser(adminEmail, "admin");
    expect(userId).toBeTruthy();
    expect(adminId).toBeTruthy();

    await page.goto("/settings/users");
    await expect(page.locator('table[aria-label="Users"]')).toBeVisible();

    // --- SEARCH ------------------------------------------------------------
    const search = page.getByRole("searchbox", { name: "Search users by email" });
    await search.fill(userEmail);

    // The "user"-role throwaway is visible; the "admin"-role one is filtered
    // out of the server-side email query.
    await expect(page.locator("tr").filter({ hasText: userEmail })).toBeVisible({ timeout: 5_000 });
    await expect(page.locator("tr").filter({ hasText: adminEmail })).toHaveCount(0);

    await search.fill("");
    await expect(page.locator("tr").filter({ hasText: userEmail })).toBeVisible({ timeout: 5_000 });

    // --- ROLE FILTER -------------------------------------------------------
    // The Role filter is a DS Select (a combobox named "Filter by role"): its
    // trigger shows the current label "Role · All". Open it and pick "Role · Admin".
    const roleFilter = page.getByRole("combobox", { name: "Filter by role" });
    await expect(roleFilter).toHaveText("Role · All");
    await roleFilter.click();
    await page.getByRole("option", { name: "Role · Admin", exact: true }).click();

    // Admin throwaway + the seeded admin@e2e.local show; the "user" throwaway
    // is hidden (client-side role filter).
    await expect(page.locator("tr").filter({ hasText: adminEmail })).toBeVisible({ timeout: 5_000 });
    await expect(page.locator("tr").filter({ hasText: "admin@e2e.local" })).toBeVisible();
    await expect(page.locator("tr").filter({ hasText: userEmail })).toHaveCount(0);

    // Reset filter back to All.
    await expect(roleFilter).toHaveText("Role · Admin");
    await roleFilter.click();
    await page.getByRole("option", { name: "Role · All", exact: true }).click();
    await expect(page.locator("tr").filter({ hasText: userEmail })).toBeVisible({ timeout: 5_000 });

    // --- PAGINATION --------------------------------------------------------
    // Users.tsx renders the pagination row only when total > PAGE(20). On the
    // e2e stack there are far fewer than 20 users, so the row is correctly
    // absent. Assert the source's actual contract: with a single page, no
    // Prev/Next controls render (and there is exactly one "N total" counter).
    const totalText = await page.locator(".users-filter-row .muted").innerText();
    const total = Number(totalText.replace(/\D/g, ""));
    expect(total).toBeGreaterThanOrEqual(3);
    if (total > 20) {
      await expect(page.getByRole("button", { name: "Prev" })).toBeVisible();
      await expect(page.getByRole("button", { name: "Next" })).toBeVisible();
    } else {
      await expect(page.getByRole("button", { name: "Prev" })).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Next" })).toHaveCount(0);
    }
  } finally {
    if (userId) await request.delete(`/api/v1/users/${userId}`, { headers: adminHeaders() });
    if (adminId) await request.delete(`/api/v1/users/${adminId}`, { headers: adminHeaders() });
  }
});
