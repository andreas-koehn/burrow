// test-only — never deploy this shape.
//
// Regression guard for the AI-gateway "empty/partial ai-config" crash class.
// GET /services/{id}/ai-config returns {} for a service that has never been
// configured. Three AI-gateway pages used to crash (blank page / React
// TypeError) when they read nested config fields on that empty blob:
//   - /gateway/providers/{slug}  → draft.routing.model_alias
//   - /cache (Semantic)   → aiConfigQuery.data.cache.semantic
//   - /inspector/{id}     → cfg.inspector.enabled
// All three now merge the response over DEFAULT_AI_CONFIG (web/src/lib/aiConfig.ts).
//
// This spec puts the seeded "ai" service into api_key mode and registers it as
// a provider (a provider is never created implicitly when the mode changes
// later) WITHOUT writing any ai-config, then visits every AI-gateway
// page and asserts none of them throws an unhandled error. The existing specs
// all configure a service first, so they never exercised this path.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

interface Service { id: string; name: string; access_mode: string }
interface AiProvider { slug: string; service_id: string }

test.use({ storageState: AUTH_STORAGE_PATH });

test("44-ai-gateway-pages: every AI-gateway page renders for an unconfigured api_key service", async ({ page, request }) => {
  // Resolve the seeded "ai" service.
  const svcRes = await request.get("/api/v1/services");
  expect(svcRes.ok()).toBeTruthy();
  const ai = ((await svcRes.json()) as Service[]).find((s) => s.name === "ai");
  expect(ai, "seeded 'ai' service must exist").toBeTruthy();
  const id = ai!.id;
  const originalMode = ai!.access_mode;

  // Collect any unhandled page error — a React render crash shows up here.
  const pageErrors: string[] = [];
  page.on("pageerror", (e) => pageErrors.push(e.message));
  // Set when this spec registered the provider itself, so it also removes it.
  let createdSlug = "";

  try {
    // Put it in api_key mode so it can back a provider. Do NOT write any
    // ai-config — that's the whole point (empty config must not crash the UI).
    const mode = await request.put(`/api/v1/services/${id}/access-mode`, {
      headers: adminHeaders(),
      data: { access_mode: "api_key" },
    });
    expect(mode.status(), "set api_key mode").toBe(204);

    // Register the provider; 409 = the service already backs one (an earlier
    // spec or the one-time backfill), which is then looked up by service id.
    const reg = await request.post("/api/v1/ai/providers", {
      headers: adminHeaders(),
      data: { slug: "e2e-ai-pages", name: "ai", kind: "tunnel", service_id: id },
    });
    expect([201, 409], "register provider").toContain(reg.status());
    if (reg.status() === 201) createdSlug = "e2e-ai-pages";
    const provRes = await request.get("/api/v1/ai/providers");
    expect(provRes.ok()).toBeTruthy();
    const slug = ((await provRes.json()) as AiProvider[]).find((p) => p.service_id === id)?.slug;
    expect(slug, "the 'ai' service must back a provider").toBeTruthy();

    // 1) Providers list — the provider must be listed now.
    await page.goto("/gateway/providers");
    await expect(page.getByRole("heading", { name: "Providers", level: 1 })).toBeVisible();
    await expect(page.locator('table[aria-label="Providers"]').locator("tr").filter({ hasText: `/ai/${slug}/v1` })).toBeVisible({ timeout: 10_000 });

    // 2) Provider detail — must render (was a blank-page crash on {}).
    await page.goto(`/gateway/providers/${slug}`);
    await expect(page.getByRole("heading", { name: /^Provider · /, level: 1 })).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole("heading", { name: "Connect a client" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Routing" })).toBeVisible();

    // 3) Prompt cache → Semantic tab — was a React TypeError ("reading 'semantic'").
    await page.goto("/cache");
    await page.getByRole("tab", { name: "Semantic" }).click();
    await expect(page.getByRole("switch", { name: "Enable semantic cache" })).toBeVisible({ timeout: 10_000 });

    // 4) Request inspector — was a React TypeError ("reading 'enabled'").
    await page.goto(`/inspector/${id}`);
    await expect(page.getByRole("heading", { name: "Request inspector", level: 1 })).toBeVisible({ timeout: 10_000 });

    // The regression guard: none of those pages threw.
    expect(pageErrors, `unexpected page errors: ${pageErrors.join(" | ")}`).toHaveLength(0);
  } finally {
    if (createdSlug) {
      await request
        .delete(`/api/v1/ai/providers/${createdSlug}`, { headers: adminHeaders() })
        .catch(() => undefined);
    }
    // Restore the original access mode so later specs are unaffected.
    await request
      .put(`/api/v1/services/${id}/access-mode`, {
        headers: adminHeaders(),
        data: { access_mode: originalMode || "open" },
      })
      .catch(() => undefined);
  }
});
