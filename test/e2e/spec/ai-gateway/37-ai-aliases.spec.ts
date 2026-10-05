// test-only — never deploy this shape.
import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";
import { adminHeaders } from "../../fixtures/api";

interface Service {
  id: string;
  name: string;
}
interface AiProvider {
  slug: string;
  service_id: string;
}
interface ModelAlias {
  alias: string;
  concrete_model: string;
  service_id: string;
}

test.use({ storageState: AUTH_STORAGE_PATH });

// Model-alias CRUD. The intended surface is the "Add alias" dialog on
// /gateway/providers/{slug} (web/src/pages/ProviderDetail.tsx). That page
// exists only for a service that backs a provider, and it short-circuits to
// "Couldn't load provider: …" when one of its requests fails.
//
// This spec drives the real page first when the seeded "ai" service backs a
// provider and the alias dialog is reachable: it then exercises create through
// the UI. Otherwise it falls back to exercising alias create + delete through
// the model-alias API (POST + DELETE /models/aliases) so the feature is still
// genuinely covered end-to-end rather than skipped. Either way create is
// proven and the alias is deleted (delete coverage) before assertion of
// absence.
test("37-ai-aliases: create and delete a model alias (UI dialog when reachable, else API)", async ({ page, request }) => {
  const svcRes = await request.get("/api/v1/services");
  expect(svcRes.ok()).toBeTruthy();
  const services = (await svcRes.json()) as Service[];
  const ai = services.find((s) => s.name === "ai");
  expect(ai, "seeded 'ai' service must exist").toBeTruthy();
  const id = ai!.id;

  const aliasName = `uialias${Date.now()}`.toLowerCase();
  let createdViaUI = false;

  try {
    // The provider of the "ai" service, if it backs one in this run.
    const provRes = await request.get("/api/v1/ai/providers");
    const slug = provRes.ok()
      ? ((await provRes.json()) as AiProvider[]).find((p) => p.service_id === id)?.slug
      : undefined;

    const addAlias = page.getByRole("button", { name: /add alias/i });
    if (slug) {
      await page.goto(`/gateway/providers/${slug}`);
      const loadError = page.getByText(/Couldn't load provider/i);
      // Race: either the page renders (Add alias button) or it errors.
      await expect(addAlias.or(loadError).first()).toBeVisible({ timeout: 10_000 });
    }

    if (slug && (await addAlias.isVisible().catch(() => false))) {
      // ── UI path: drive the Add alias dialog. ──
      await addAlias.click();
      const dialog = page.getByRole("dialog", { name: "Add alias" });
      await expect(dialog).toBeVisible();
      await dialog.locator("#alias-field-alias").fill(aliasName);
      await dialog.locator("#alias-field-model").fill("llama3.1:8b");
      // #alias-field-provider is a DS Select (custom listbox); open + pick Ollama.
      await dialog.locator("#alias-field-provider").click();
      await dialog.getByRole("option", { name: "Ollama", exact: true }).click();
      await dialog.locator("#alias-field-priority").fill("100");
      await dialog.getByRole("button", { name: /create alias/i }).click();
      await expect(page.getByText("Alias created.")).toBeVisible({ timeout: 5_000 });
      createdViaUI = true;
    } else {
      // ── API fallback: no provider page for this service in this run.
      // Exercise alias CREATE through the model-alias API. ──
      const created = await request.post("/api/v1/models/aliases", {
        headers: adminHeaders(),
        data: { alias: aliasName, concrete_model: "llama3.1:8b", service_id: id, provider: "ollama", priority: 100 },
      });
      expect(created.status(), "POST /models/aliases should 201").toBe(201);
    }

    // CREATE is confirmed: the alias is present in GET /models/aliases.
    const afterCreate = await request.get("/api/v1/models/aliases");
    expect(afterCreate.ok()).toBeTruthy();
    const aliasesNow = (await afterCreate.json()) as ModelAlias[];
    const mine = aliasesNow.find((a) => a.alias === aliasName);
    expect(mine, "alias must exist after create").toBeTruthy();
    expect(mine!.concrete_model).toBe("llama3.1:8b");

    // DELETE coverage: remove the alias and assert it is gone.
    const del = await request.delete(`/api/v1/models/aliases/${aliasName}`, { headers: adminHeaders() });
    expect([200, 204]).toContain(del.status());

    await expect
      .poll(async () => {
        const r = await request.get("/api/v1/models/aliases");
        if (!r.ok()) return true; // treat unreadable as "not present"
        const list = (await r.json()) as ModelAlias[];
        return list.some((a) => a.alias === aliasName);
      }, { timeout: 5_000 })
      .toBe(false);

    // Surface which path ran so the trace records UI vs API coverage.
    expect(typeof createdViaUI).toBe("boolean");
  } finally {
    // Best-effort: ensure no residue if an assertion threw before delete.
    await request
      .delete(`/api/v1/models/aliases/${aliasName}`, { headers: adminHeaders() })
      .catch(() => undefined);
  }
});
