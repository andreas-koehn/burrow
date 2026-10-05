// test-only — never deploy this shape.
//
// Spec 50 — Account › Active sessions: per-session Revoke + Sign out everywhere.
//
// CRITICAL ISOLATION: every destructive session op here runs against a
// THROWAWAY user inside its own browser context(s). We never touch the shared
// admin storageState (playwright-auth.json) — revoking the admin's session
// would corrupt the auth fixture and break every other spec. The admin session
// is used ONLY (via the `request` fixture) to create/delete the throwaway user.
//
// Backend contract (internal/api/session_handlers.go):
//   GET    /sessions               → [{id, ip, user_agent, created_at, expires_at, current}]
//   DELETE /sessions/{id}          → revoke one session
//   POST   /sessions/revoke-all    → RevokeOtherSessions (keeps the CURRENT
//                                     session; "Sign out everywhere" signs out
//                                     OTHERS, not the caller).
import { test, expect } from "@playwright/test";
import type { BrowserContext } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

// `request` carries the admin session so we can create/delete the throwaway
// user. The browser contexts below log in fresh as that user.
test.use({ storageState: AUTH_STORAGE_PATH });

async function loginUser(ctx: BrowserContext, email: string, password: string) {
  const p = await ctx.newPage();
  await p.goto("/login");
  await p.fill("#login-email", email);
  await p.fill("#login-password", password);
  await p.getByRole("button", { name: "Sign in", exact: true }).click();
  await p.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 10_000 });
  return p;
}

test("50-session-management: revoke a non-current session + sign out everywhere", async ({ browser, request }) => {
  const email = `sess-${Date.now()}@e2e.local`;
  const password = "init-pass-123";

  const created = await request.post("/api/v1/users", {
    headers: adminHeaders(),
    data: { email, password, role: "user" },
  });
  expect(created.status()).toBe(201);
  const userId = (await created.json()).id as string;
  expect(userId).toBeTruthy();

  // Two independent contexts → two distinct sessions for the throwaway user.
  const ctx1 = await browser.newContext({ baseURL: "http://localhost:8080" });
  const ctx2 = await browser.newContext({ baseURL: "http://localhost:8080" });

  try {
    const p1 = await loginUser(ctx1, email, password);
    await loginUser(ctx2, email, password);

    // --- Context 1: open the sessions page and inspect the sessions table. ---
    await p1.goto("/settings/sessions");
    const sessionsTable = p1.getByRole("table", { name: "Active sessions" });
    await expect(sessionsTable).toBeVisible();

    const rows = sessionsTable.locator("tbody tr");
    // Two logins → at least two sessions for this user.
    await expect(async () => {
      expect(await rows.count()).toBeGreaterThanOrEqual(2);
    }).toPass({ timeout: 10_000 });

    // Exactly one row is the current device (marked "THIS DEVICE").
    const currentRow = rows.filter({ hasText: "THIS DEVICE" });
    await expect(currentRow).toHaveCount(1);

    const beforeCount = await rows.count();

    // --- Step 3: revoke a NON-current session via its row's "Revoke" button.
    // Current rows show "—" in the actions cell (no Revoke button); only the
    // other session(s) expose Revoke.
    const revokeBtn = sessionsTable.getByRole("button", { name: "Revoke" }).first();
    await expect(revokeBtn).toBeVisible();
    await revokeBtn.click();
    await expect(p1.getByText("Session revoked")).toBeVisible({ timeout: 5_000 });

    // Row count drops by one after the revoke (list re-fetches on success).
    await expect(async () => {
      expect(await rows.count()).toBe(beforeCount - 1);
    }).toPass({ timeout: 10_000 });

    // Current device row is still present.
    await expect(rows.filter({ hasText: "THIS DEVICE" })).toHaveCount(1);

    // --- Step 4: "Sign out everywhere" revokes OTHER sessions, keeps current.
    // To assert it has something to revoke, log in a THIRD time first.
    const ctx3 = await browser.newContext({ baseURL: "http://localhost:8080" });
    await loginUser(ctx3, email, password);

    // Re-fetch the list in context 1 so it sees the new session, then sign out
    // everywhere.
    await p1.reload();
    await expect(sessionsTable).toBeVisible();
    await expect(async () => {
      expect(await rows.count()).toBeGreaterThanOrEqual(2);
    }).toPass({ timeout: 10_000 });

    await p1.getByRole("button", { name: "Sign out everywhere" }).click();
    await expect(p1.getByText("Signed out everywhere")).toBeVisible({ timeout: 5_000 });

    // After "Sign out everywhere", only the current session remains: exactly
    // one row, and it is the current device. (RevokeOtherSessions keeps caller.)
    await expect(async () => {
      expect(await rows.count()).toBe(1);
    }).toPass({ timeout: 10_000 });
    await expect(rows.filter({ hasText: "THIS DEVICE" })).toHaveCount(1);

    await ctx3.close();
  } finally {
    await ctx1.close();
    await ctx2.close();
    await request.delete(`/api/v1/users/${userId}`, { headers: adminHeaders() });
  }
});
