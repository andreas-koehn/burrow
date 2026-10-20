// test-only — never deploy this shape.
// CSRF-aware helpers for admin API calls via the Playwright request fixture.
// The Playwright `request` fixture sends the session cookie from storageState
// but does NOT echo the `burrow_csrf` cookie as X-CSRF-Token. This module
// reads the token from the saved storage and returns the required headers.

import * as fs from "node:fs";
import * as path from "node:path";

// process.cwd() === test/e2e when Playwright runs (same pattern as cert.ts).
const DEFAULT_STORAGE_PATH = "playwright-auth.json";

export function readCSRFFromStorage(storagePath = DEFAULT_STORAGE_PATH): string {
  const abs = path.resolve(process.cwd(), storagePath);
  const data = JSON.parse(fs.readFileSync(abs, "utf8")) as {
    cookies: { name: string; value: string }[];
  };
  const ck = data.cookies?.find((c) => c.name === "burrow_csrf" || c.name === "__Host-burrow_csrf");
  if (!ck?.value)
    throw new Error(
      "burrow_csrf (or __Host-burrow_csrf) not in playwright-auth.json — run 01-bootstrap first",
    );
  return ck.value;
}

/** Default headers (incl. X-CSRF-Token) for admin API calls via the Playwright request fixture. */
export function adminHeaders(): Record<string, string> {
  return {
    "X-CSRF-Token": readCSRFFromStorage(),
    "Content-Type": "application/json",
  };
}
