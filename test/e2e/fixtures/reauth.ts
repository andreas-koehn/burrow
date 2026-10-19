// test-only — never deploy this shape.
// The stack runs without BURROW_SERVICES_DOMAIN, so minting secrets and admin
// writes need a recent password confirmation (T02b): POST /auth/reauth opens
// a 10-minute window for the session. The window lives in the relay's memory,
// so a relay restart closes it. Each confirmation costs a slot of the
// per-address sign-in limit, so this confirms at most every 8 minutes per
// worker, or again after forgetReauth().
import type { APIRequestContext, Page } from "@playwright/test";
import { ADMIN_PASSWORD } from "./env";
import { readCSRFFromStorage } from "./api";

const REFRESH_MS = 8 * 60 * 1000;
let confirmedAt = 0;

/** Opens (or keeps open) the re-authentication window of the shared admin session. */
export async function ensureReauth(request: APIRequestContext): Promise<void> {
  if (Date.now() - confirmedAt < REFRESH_MS) return;
  const send = () => request.post("/api/v1/auth/reauth", {
    headers: { "X-CSRF-Token": readCSRFFromStorage(), "Content-Type": "application/json" },
    data: { password: ADMIN_PASSWORD },
  });
  let res = await send();
  if (res.status() === 429) {
    // The sign-in limit counts per minute: wait it out once instead of
    // adding attempts that keep the bucket full.
    await new Promise((r) => setTimeout(r, 61_000));
    res = await send();
  }
  if (!res.ok()) throw new Error(`reauth: ${res.status()} ${await res.text()}`);
  confirmedAt = Date.now();
}

/** The relay restarted: the next ensureReauth confirms again. */
export function forgetReauth(): void {
  confirmedAt = 0;
}

/**
 * For a session of its own (a UI sign-in): if the "Confirm your password"
 * dialog opens after an action, answer it.
 */
export async function answerReauthDialog(page: Page, password: string, timeout = 3_000): Promise<void> {
  const dialog = page.getByRole("dialog", { name: "Confirm your password" });
  try {
    await dialog.waitFor({ state: "visible", timeout });
  } catch {
    return;
  }
  await dialog.getByLabel("Your password").fill(password);
  await dialog.getByRole("button", { name: "Confirm" }).click();
  await dialog.waitFor({ state: "hidden" });
}
