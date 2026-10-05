// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

// The `request` fixture carries the admin session (storageState) so we can
// create/delete the throwaway user via the admin API. The browser logins below
// use fresh, isolated contexts so we never mutate the admin session's password.
test.use({ storageState: AUTH_STORAGE_PATH });

test("34-account-password: user changes own password via /settings/profile, then logs in with the new one", async ({ browser, request }) => {
  const email = `pwuser-${Date.now()}@e2e.local`;
  const initPass = "init-pass-123";
  const newPass = "new-pass-456";

  const created = await request.post("/api/v1/users", {
    headers: adminHeaders(),
    data: { email, password: initPass, role: "user" },
  });
  expect(created.status()).toBe(201);
  const userId = (await created.json()).id as string;
  expect(userId).toBeTruthy();

  try {
    // --- Context 1: log in as the throwaway user and change the password. ---
    const ctx1 = await browser.newContext({ baseURL: "http://localhost:8080" });
    const p1 = await ctx1.newPage();
    await p1.goto("/login");
    await p1.fill("#login-email", email);
    await p1.fill("#login-password", initPass);
    await p1.getByRole("button", { name: "Sign in", exact: true }).click();
    await p1.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 10_000 });

    await p1.goto("/settings/profile");
    await p1.fill("#current-password", initPass);
    await p1.fill("#new-password", newPass);
    await p1.fill("#confirm-password", newPass);
    await p1.getByRole("button", { name: "Change password" }).click();
    await expect(p1.getByText("Password changed successfully")).toBeVisible({ timeout: 5_000 });
    await ctx1.close();

    // --- Context 2: a fresh login must succeed with the NEW password. ---
    const ctx2 = await browser.newContext({ baseURL: "http://localhost:8080" });
    const p2 = await ctx2.newPage();
    await p2.goto("/login");
    await p2.fill("#login-email", email);
    await p2.fill("#login-password", newPass);
    await p2.getByRole("button", { name: "Sign in", exact: true }).click();
    await p2.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 10_000 });
    await ctx2.close();
  } finally {
    await request.delete(`/api/v1/users/${userId}`, { headers: adminHeaders() });
  }
});
