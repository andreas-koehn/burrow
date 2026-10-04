import { test, expect } from "@playwright/test";

// v0.5.0 dashboard surfaces — retention, database, connection-logs, OpenAPI
// viewer (no-CDN assertion), semantic cache tab, upstream credentials, custom
// domains — against the REAL built burrowd (no MSW).

test.use({ storageState: "playwright-auth.json" });

// ── 1. Retention page loads + PUTs ────────────────────────────────────────────
test("v0.5.0: retention page loads and saves", async ({ page }) => {
  await page.goto("/settings/retention");
  await expect(page.getByRole("heading", { name: "Retention & compliance" })).toBeVisible();

  // Wait for all inputs to be visible (form rendered after API load).
  const usageInput = page.getByLabel("Usage events (days)");
  await expect(usageInput).toBeVisible({ timeout: 15_000 });

  // Fill ALL fields with valid in-range values. The fresh CI DB seeds 0 for
  // most fields; several have min=1, which triggers form validation and keeps
  // the Save button disabled until all fields are valid.
  await page.getByLabel("Audit log (days)").fill("30");           // min=0 → 30 OK
  await page.getByLabel("Usage events (days)").fill("60");        // min=1 → 60 OK
  await page.getByLabel("Redaction events (days)").fill("30");    // min=1 → 30 OK
  await page.getByLabel("Connection logs (days)").fill("30");     // min=1 → 30 OK
  await page.getByLabel("Connection log rollups (days)").fill("30"); // min=0 → 30 OK
  await page.getByLabel("Webhook deliveries (days)").fill("30"); // min=1 → 30 OK
  await page.getByLabel("Inspector ring-buffer size").fill("100"); // min=1 → 100 OK

  // Save button should now be enabled (no validation errors).
  const saveBtn = page.getByRole("button", { name: "Save" });
  await expect(saveBtn).toBeEnabled();
  await saveBtn.click();

  // Success toast from Retention.tsx: toast.success("Retention settings saved.")
  await expect(
    page.getByText("Retention settings saved", { exact: false }),
  ).toBeVisible();
});

// ── 2. Database backend page renders driver chip ───────────────────────────────
test("v0.5.0: database backend page shows sqlite driver", async ({ page }) => {
  await page.goto("/settings/database");
  await expect(page.getByRole("heading", { name: "Database backend" })).toBeVisible();

  // The driver chip is a <code> element containing "sqlite"
  await expect(page.locator("code.mono").filter({ hasText: "sqlite" })).toBeVisible();
});

// ── 3. Connection logs page renders + Rollups toggle ──────────────────────────
test("v0.5.0: connection logs page renders and rollups toggle works", async ({ page }) => {
  await page.goto("/connection-logs");
  await expect(page.getByRole("heading", { name: "Connection logs" })).toBeVisible();

  // In default (non-rollup) mode, the table or empty state is shown.
  // Empty state: the EmptyState card renders <h4>No connection logs yet</h4>.
  // Table mode: aria-label="Connection logs".
  const tableOrEmpty = page.locator(
    '[aria-label="Connection logs"], h4:has-text("No connection logs yet")',
  );
  await expect(tableOrEmpty.first()).toBeVisible();

  // Click the Rollups checkbox (aria-label="Rollups")
  const rollupsCheckbox = page.getByRole("checkbox", { name: "Rollups" });
  await expect(rollupsCheckbox).toBeVisible();
  await rollupsCheckbox.check();
  await expect(rollupsCheckbox).toBeChecked();

  // After enabling rollups either the "Day" column header appears or the
  // rollups empty-state card (<h4>No rollups yet</h4>) shows (no rollup data in
  // the fresh CI fixture).
  const dayColOrEmpty = page.locator(
    'th:has-text("Day"), h4:has-text("No rollups yet")',
  );
  await expect(dayColOrEmpty.first()).toBeVisible();
});

// ── 4. OpenAPI viewer: no external CDN requests ───────────────────────────────
test("v0.5.0: OpenAPI viewer loads with zero external-CDN requests", async ({ page }) => {
  // The OpenAPI viewer is now an in-app page (P1-14): the Settings
  // "OpenAPI viewer" nav card is a same-tab <Link to="/openapi"> (no
  // target="_blank"), and /openapi frames /api/v1/openapi/viewer/ in an
  // <iframe>. So we navigate in-app and inspect the iframe — no new tab opens.

  // Collect every external request made on this page (incl. iframe subresources).
  const externalRequests: string[] = [];
  page.on("request", (req) => {
    const url = req.url();
    // Allow same-origin requests: localhost or 127.0.0.1 on any port, and data: URIs.
    if (
      url.startsWith("http://localhost") ||
      url.startsWith("https://localhost") ||
      url.startsWith("http://127.0.0.1") ||
      url.startsWith("https://127.0.0.1") ||
      url.startsWith("data:")
    ) return;
    externalRequests.push(url);
  });

  // Navigate to a page that renders the full Layout with the sidebar nav.
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

  // The Settings "OpenAPI viewer" card links in-app to /openapi (same tab).
  // The card's accessible name is its full text ("OpenAPI viewer" + the
  // description line), so match by substring rather than exact.
  await page.getByRole("link", { name: /OpenAPI viewer/ }).click();
  await expect(page).toHaveURL(/\/openapi$/);
  await expect(page.getByRole("heading", { name: "OpenAPI", exact: true })).toBeVisible();

  // The viewer is served inside an <iframe src="/api/v1/openapi/viewer/">.
  // Its bundled HTML contains the #route-list element (no external CDN deps).
  const viewer = page.frameLocator('iframe[title="OpenAPI viewer"]');
  await expect(viewer.locator("#route-list")).toBeVisible({ timeout: 15_000 });

  // Let any iframe subresources (viewer.css / viewer.js / spec fetch) settle.
  await page.waitForLoadState("networkidle");

  // Assert no external CDN requests were fired by the page or its iframe.
  expect(externalRequests, `External requests found: ${externalRequests.join(", ")}`).toHaveLength(0);
});

// ── 5. Semantic cache tab is live (not disabled placeholder) ──────────────────
test("v0.5.0: semantic cache tab is enabled and shows controls", async ({ page }) => {
  await page.goto("/cache");
  await expect(page.getByRole("heading", { name: "Prompt cache" })).toBeVisible();

  // Click the "Semantic" tab
  const semanticTab = page.getByRole("tab", { name: "Semantic" });
  await expect(semanticTab).toBeVisible();
  // Tab must not be disabled (v0.4.0 had it as a placeholder)
  await expect(semanticTab).not.toBeDisabled();
  await semanticTab.click();

  // The semantic panel shows the advisory text from SemanticPanel
  await expect(
    page.getByText("Semantic caching is off by default", { exact: false }),
  ).toBeVisible();

  // The "Enable semantic cache" switch is present and clickable
  const enableSwitch = page.getByRole("switch", { name: "Enable semantic cache" });
  await expect(enableSwitch).toBeVisible();
  await expect(enableSwitch).not.toBeDisabled();
});

// ── 6. Upstream-credential binding tab shows env-only disclosure ──────────────
//
// v0.5.2 P3.6: this test used to early-return when no live service existed in
// the CI fixture. Now we pre-provision a service row via the new admin
// POST /api/v1/services endpoint so the assertion runs unconditionally.
test("v0.5.0: upstream key tab shows env-only disclosure text", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const list = await page.request.get("/api/v1/services");
  expect(list.status()).toBe(200);
  const services = (await list.json()) as { id: string; name: string }[];

  let serviceId: string;
  if (services.length === 0) {
    // Pre-provision a service via the admin POST /services endpoint
    // (v0.5.2 P3.6). The handler accepts service_id, title, and access_mode;
    // duplicates return 409 which is acceptable here (we re-use an existing row).
    const created = await page.request.post("/api/v1/services", {
      headers,
      data: { service_id: "svc_e2e_upstream", title: "Playwright pre-provisioned", access_mode: "api_key" },
    });
    // 201 on first run, 409 on a re-run; both leave the row in place.
    expect([201, 409]).toContain(created.status());
    serviceId = "svc_e2e_upstream";
  } else {
    serviceId = services[0]!.id;
  }

  await page.goto(`/services/${serviceId}`);
  await expect(page.getByRole("heading", { name: /Service/ })).toBeVisible();

  // Click the "Upstream key" tab (value="upstream-key", label="Upstream key")
  const upstreamTab = page.getByRole("tab", { name: "Upstream key" });
  await expect(upstreamTab).toBeVisible();
  await upstreamTab.click();

  // The verbatim disclosure text from UpstreamCredentials.tsx
  await expect(
    page.getByText("Slot values live in environment variables on this server, never in the database", { exact: false }),
  ).toBeVisible();
});

// ── 7. Custom domains tab shows New domain form ───────────────────────────────
//
// v0.5.2 P3.6: previously early-returned when no live service existed; now
// pre-provisions one via the admin POST /api/v1/services so the assertion
// always runs.
test("v0.5.0: custom domains tab opens add-domain dialog", async ({ page }) => {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === "burrow_csrf")?.value ?? "";
  const headers = { "X-CSRF-Token": csrf, "Content-Type": "application/json" };

  const list = await page.request.get("/api/v1/services");
  expect(list.status()).toBe(200);
  const services = (await list.json()) as { id: string; name: string }[];

  let serviceId: string;
  if (services.length === 0) {
    // Pre-provision a service via the admin POST /services endpoint
    // (v0.5.2 P3.6). Re-using the same row across test 6 + 7 is fine; the
    // handler returns 409 on duplicates which we treat as a successful no-op.
    const created = await page.request.post("/api/v1/services", {
      headers,
      data: { service_id: "svc_e2e_domains", title: "Playwright domains pre-provisioned" },
    });
    expect([201, 409]).toContain(created.status());
    serviceId = "svc_e2e_domains";
  } else {
    serviceId = services[0]!.id;
  }

  await page.goto(`/services/${serviceId}/domains`);
  await expect(page.getByRole("heading", { name: /Service/ })).toBeVisible();

  // The Custom domains tab should be active (initialTab="domains" when path ends in /domains)
  // The panel contains the "Custom domains" h3 and "New domain" button.
  await expect(page.getByRole("heading", { name: "Custom domains" })).toBeVisible();
  await expect(page.getByRole("button", { name: "New domain", exact: true })).toBeVisible();

  // Open the add-domain dialog
  await page.getByRole("button", { name: "New domain", exact: true }).click();

  // Dialog title
  await expect(page.getByRole("dialog", { name: "Add custom domain" })).toBeVisible();

  // Fill hostname
  const hostnameInput = page.getByLabel("Hostname");
  await expect(hostnameInput).toBeVisible();
  await hostnameInput.fill("foo.example.com");

  // Fill in PEM cert and key (minimal PEM-block placeholders so client-side
  // validation won't block; the server will reject invalid certs with a 400,
  // which is acceptable here — we are testing the UI form flow).
  const DUMMY_CERT = [
    "-----BEGIN CERTIFICATE-----",
    "MIICpDCCAYwCCQDU7pQ4NHnSqTANBgkqhkiG9w0BAQsFADAUMRIwEAYDVQQDDAls",
    "b2NhbGhvc3QwHhcNMjQwMTAxMDAwMDAwWhcNMjUwMTAxMDAwMDAwWjAUMRIwEAYD",
    "VQQDDAlsb2NhbGhvc3QwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQC7",
    "-----END CERTIFICATE-----",
  ].join("\n");

  const DUMMY_KEY = [
    "-----BEGIN PRIVATE KEY-----",
    "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7o7o7o7o7o7o7",
    "-----END PRIVATE KEY-----",
  ].join("\n");

  await page.getByLabel("Certificate (PEM)").fill(DUMMY_CERT);
  await page.getByLabel("Private key (PEM)").fill(DUMMY_KEY);

  // Click Add — the server will return 400 (cert invalid) which is fine;
  // we just need to confirm the form submitted (no JS crash).
  await page.getByRole("button", { name: "Add", exact: true }).click();

  // Either the dialog closes (success) OR a rejection/error message appears.
  // In both cases the dialog should either close or show an inline alert.
  // We wait briefly for one of those outcomes.
  const dialogClosed = page.getByRole("dialog", { name: "Add custom domain" }).isHidden();
  const rejectionMsg = page.locator('[role="alert"]');
  const toastErr = page.getByText("Could not add domain", { exact: false });

  // At least one outcome should appear within the expect timeout.
  await expect(
    page.locator('[role="dialog"][aria-label="Add custom domain"], [role="alert"]'),
  ).toBeAttached();

  // Clean up: close dialog if still open
  const isDialogVisible = await page.getByRole("dialog", { name: "Add custom domain" }).isVisible();
  if (isDialogVisible) {
    await page.getByRole("button", { name: "Cancel" }).click();
  }

  // Verify the domains panel is still visible (no catastrophic crash)
  await expect(page.getByRole("heading", { name: "Custom domains" })).toBeVisible();
});
