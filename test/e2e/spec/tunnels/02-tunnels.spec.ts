// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { pingTcpTunnel } from "../../fixtures/traffic";

test.use({ storageState: AUTH_STORAGE_PATH });

test("02-tunnels: tcp-echo bytes_in AND bytes_out counters increment via SSE under traffic", async ({ page, request }) => {
  await page.goto("/services?live=1");
  const row = page.locator('table[aria-label="Services"] tr').filter({ hasText: "tcp-echo" });
  await expect(row).toBeVisible();
  // After the IN/OUT merge into TRAFFIC (S6), both byte counters live in
  // .col-traffic. Select by the stable ASCII title attribute rather than the
  // Unicode arrow glyph: <span title="In: N bytes"> / <span title="Out: N bytes">.
  const bytesIn = row.locator('td.col-traffic span[title^="In:"]');
  const bytesOut = row.locator('td.col-traffic span[title^="Out:"]');
  const initialIn = (await bytesIn.innerText()).trim();
  const initialOut = (await bytesOut.innerText()).trim();

  await pingTcpTunnel(request, 5);

  // Both counters must move within the SSE refresh window. The "tunnels" SSE
  // event invalidates the ["tunnels"] query, which re-fetches stats.
  await expect.poll(
    async () => (await bytesIn.innerText()).trim(),
    { timeout: 10_000, message: "bytes_in counter did not change under traffic" },
  ).not.toBe(initialIn);

  await expect.poll(
    async () => (await bytesOut.innerText()).trim(),
    { timeout: 10_000, message: "bytes_out counter did not change under traffic" },
  ).not.toBe(initialOut);
});
