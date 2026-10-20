import { expect, type Locator, type Page } from "@playwright/test";

// The e2e relay runs without BURROW_SERVICES_DOMAIN, so minting secrets and
// admin writes need a recent password confirmation ("sudo mode", T02b):
// without one the relay answers 403 reauth_required and the dashboard opens
// its "Confirm your password" dialog.

export const ADMIN_PASSWORD = "e2e-password-123";

export async function csrfHeaders(page: Page): Promise<Record<string, string>> {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf")?.value ?? "";
  return { "X-CSRF-Token": csrf, "Content-Type": "application/json" };
}

/**
 * Opens the 10-minute re-authentication window for the page's session through
 * the API, for setup code that calls guarded routes with page.request.
 */
export async function confirmPassword(page: Page, password = ADMIN_PASSWORD): Promise<void> {
  const res = await page.request.post("/api/v1/auth/reauth", {
    headers: await csrfHeaders(page),
    data: { password },
  });
  expect(res.status(), await res.text()).toBeLessThan(300);
}

/**
 * Sends a request with the session's CSRF header. On 403 reauth_required it
 * confirms the password once and repeats the request: the shared session's
 * window from global-setup normally covers the suite, and each confirmation
 * costs a slot of the per-address sign-in limit.
 */
export async function guardedRequest(page: Page, method: string, url: string, data?: unknown) {
  const send = async () => page.request.fetch(url, { method, headers: await csrfHeaders(page), data });
  let res = await send();
  if (res.status() === 403 && (await res.text()).includes("reauth_required")) {
    await confirmPassword(page);
    res = await send();
  }
  return res;
}

/**
 * Call right after a UI action that may hit a guarded route: if the
 * "Confirm your password" dialog opens, answer it; then wait for `done`. The
 * window may already be open from an earlier spec on the same session, in
 * which case no dialog appears.
 */
export async function answerReauth(page: Page, done: Locator, password = ADMIN_PASSWORD): Promise<void> {
  const dialog = page.getByRole("dialog", { name: "Confirm your password" });
  await expect(dialog.or(done).first()).toBeVisible();
  if (await dialog.isVisible()) {
    await dialog.getByLabel("Your password").fill(password);
    await dialog.getByRole("button", { name: "Confirm" }).click();
    await expect(dialog).toBeHidden();
  }
  await expect(done).toBeVisible();
}
