#!/usr/bin/env node
/**
 * capture-screenshots.mjs
 *
 * Captures full-page screenshots of every Burrow UI view + key sub-views +
 * representative modals in both light and dark themes at 1920×1080.
 *
 * Run from repo root:
 *   node tools/capture-screenshots.mjs
 *
 * Requires the 4-container e2e lab to be UP:
 *   task e2e:lab
 * (admin@e2e.local / e2e-pass at http://localhost:8080)
 */

// Resolve Playwright from test/e2e/node_modules so the repo root needs no
// extra npm install. Using createRequire lets us target a specific module tree.
import { createRequire } from "node:module";
import path from "node:path";
import fs from "node:fs";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const repoRoot = path.resolve(__dirname, "..");

const require = createRequire(path.join(repoRoot, "test/e2e/package.json"));
const { chromium } = require("playwright");

// ─── Config ─────────────────────────────────────────────────────────────────
const BASE_URL = "http://localhost:8080";
const ADMIN_EMAIL = "admin@e2e.local";
const ADMIN_PASSWORD = "e2e-pass";
const VIEWPORT = { width: 1920, height: 1080 };
const OUT_DIR = path.join(repoRoot, "screenshots");

// ─── Helpers ─────────────────────────────────────────────────────────────────

async function applyTheme(page, isDark) {
  // Belt-and-suspenders: set localStorage key AND toggle the .dark class on
  // the root element. The app reads "burrow-theme" on mount (theme-provider.tsx).
  await page.evaluate((dark) => {
    localStorage.setItem("burrow-theme", dark ? "dark" : "light");
    document.documentElement.classList.toggle("dark", dark);
  }, isDark);
}

async function navigateAndSettle(page, url, isDark) {
  // Use "domcontentloaded" rather than "networkidle" because several pages
  // open a persistent SSE connection (/api/v1/events) that keeps the network
  // permanently busy, causing networkidle to time out after 30 s.
  await page.goto(`${BASE_URL}${url}`, { waitUntil: "domcontentloaded" });
  // After navigation React re-mounts; re-apply theme via DOM so it takes
  // effect regardless of any re-render cycle.
  await applyTheme(page, isDark);
  // Give React + data fetches time to settle.
  await page.waitForTimeout(1200);
}

async function loginOnce(page) {
  await page.goto(`${BASE_URL}/login`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#login-email", { timeout: 10_000 });
  await page.fill("#login-email", ADMIN_EMAIL);
  await page.fill("#login-password", ADMIN_PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 15_000 });
  await page.waitForTimeout(600);
}

// Fetch the first AI/http service ID from the API (needed for /inspector/:id).
async function fetchFirstAiServiceId(page) {
  try {
    const resp = await page.evaluate(async (base) => {
      const r = await fetch(`${base}/api/v1/services`);
      return r.ok ? r.json() : [];
    }, BASE_URL);
    const svc = resp.find((s) => s.type === "http");
    return svc?.id ?? null;
  } catch {
    return null;
  }
}

// Fetch the first AI endpoint id from the API.
async function fetchFirstAiEndpointId(page) {
  try {
    const resp = await page.evaluate(async (base) => {
      const r = await fetch(`${base}/api/v1/ai/endpoints`);
      return r.ok ? r.json() : [];
    }, BASE_URL);
    return resp[0]?.id ?? null;
  } catch {
    return null;
  }
}

function ensureDir(dir) {
  if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });
}

// ─── Capture targets ──────────────────────────────────────────────────────────
//
// Each target: { nn, name, url, before?, waitFor? }
//   nn     : zero-padded 2-digit number string used in filename
//   name   : slug used in filename
//   url    : path to navigate to (relative to BASE_URL)
//   before : optional async (page, ctx) => ... runs after navigation+settle
//   waitFor: optional selector to wait for visibility before screenshot

function buildTargets(aiServiceId, aiEndpointId) {
  const insp = aiServiceId ? `/inspector/${aiServiceId}` : null;
  const svcDetail = aiServiceId ? `/services/${aiServiceId}` : null;

  return [
    // ── Auth + main views ────────────────────────────────────────────────────
    {
      nn: "01", name: "login",
      url: "/login",
      loggedOut: true,             // capture while logged out
    },
    {
      nn: "02", name: "clients",
      url: "/clients",
      waitFor: 'h1:has-text("Clients")',
    },
    {
      nn: "03", name: "tunnels",
      url: "/tunnels",
      waitFor: 'h1:has-text("Tunnels")',
    },
    {
      nn: "04", name: "services",
      url: "/services",
      waitFor: 'h1:has-text("Services")',
    },
    {
      nn: "05", name: "tokens",
      url: "/tokens",
      waitFor: 'h1:has-text("Client tokens")',
    },
    {
      nn: "06", name: "ai-endpoints",
      url: "/ai/endpoints",
      waitFor: 'h1:has-text("AI endpoints")',
    },
    {
      nn: "07", name: "cost",
      url: "/cost",
      waitFor: 'h1:has-text("Cost")',
    },
    {
      nn: "08", name: "cache",
      url: "/cache",
      waitFor: 'h1:has-text("Prompt cache")',
    },
    {
      nn: "09", name: "guardrails",
      url: "/guardrails",
      waitFor: 'h1:has-text("Guardrails")',
    },
    {
      nn: "10", name: "inspector",
      url: insp ?? "/ai/endpoints",
      waitFor: insp ? 'h1:has-text("Inspector")' : 'h1:has-text("AI endpoints")',
      note: insp ? null : "no AI service — captured /ai/endpoints as fallback",
    },
    {
      nn: "11", name: "users",
      url: "/users",
      waitFor: 'h1:has-text("Users")',
    },
    {
      nn: "12", name: "roles",
      url: "/roles",
      waitFor: 'h1:has-text("Roles")',
    },
    {
      nn: "13", name: "settings",
      url: "/settings",
      waitFor: 'h1:has-text("Settings")',
    },
    {
      nn: "14", name: "audit",
      url: "/audit",
      waitFor: 'h1:has-text("Audit log")',
    },
    {
      nn: "15", name: "webhooks",
      url: "/webhooks",
      waitFor: 'h1:has-text("Webhooks")',
    },
    {
      nn: "16", name: "account",
      url: "/account",
      waitFor: 'h1:has-text("Account")',
    },
    {
      nn: "17", name: "automation",
      url: "/account/automation",
      waitFor: 'h1:has-text("Automation")',
    },

    // ── Sub-views ────────────────────────────────────────────────────────────
    {
      nn: "18", name: "clients-connect",
      url: "/clients/connect",
      waitFor: 'h1:has-text("Connect a client")',
    },
    {
      nn: "19", name: "clients-connect-revealed",
      url: "/clients/connect",
      waitFor: 'h1:has-text("Connect a client")',
      before: async (page) => {
        // Fill name and click Generate token, then Reveal
        await page.fill('input[aria-label="Client name"]', "screenshot-probe");
        await page.getByRole("button", { name: "Generate token" }).click();
        // Wait for the credentials section to appear
        await page.waitForSelector('h2:has-text("Credentials")', { timeout: 10_000 });
        await page.waitForTimeout(400);
        // Click Reveal
        const reveal = page.getByRole("button", { name: /reveal/i });
        if (await reveal.count() > 0) {
          await reveal.first().click();
          await page.waitForTimeout(300);
        }
      },
    },
    {
      nn: "20", name: "service-detail-access",
      url: svcDetail ?? "/services",
      waitFor: svcDetail ? 'h1:has-text("Service")' : 'h1:has-text("Services")',
    },
    {
      nn: "21", name: "service-detail-api-keys",
      url: svcDetail ?? "/services",
      waitFor: svcDetail ? 'h1:has-text("Service")' : 'h1:has-text("Services")',
      before: async (page) => {
        if (!svcDetail) return;
        const apiKeysTab = page.getByRole("tab", { name: "API keys" });
        if (await apiKeysTab.count() > 0) {
          await apiKeysTab.click();
          await page.waitForTimeout(400);
        }
      },
    },
    {
      nn: "22", name: "service-detail-upstream-key",
      url: svcDetail ?? "/services",
      waitFor: svcDetail ? 'h1:has-text("Service")' : 'h1:has-text("Services")',
      before: async (page) => {
        if (!svcDetail) return;
        const upstreamTab = page.getByRole("tab", { name: "Upstream key" });
        if (await upstreamTab.count() > 0) {
          await upstreamTab.click();
          await page.waitForTimeout(400);
        }
      },
    },
    {
      nn: "23", name: "service-detail-custom-domains",
      url: svcDetail ?? "/services",
      waitFor: svcDetail ? 'h1:has-text("Service")' : 'h1:has-text("Services")',
      before: async (page) => {
        if (!svcDetail) return;
        const domainsTab = page.getByRole("tab", { name: "Custom domains" });
        if (await domainsTab.count() > 0) {
          await domainsTab.click();
          await page.waitForTimeout(400);
        }
      },
    },
    {
      nn: "24", name: "ai-endpoint-detail",
      url: aiEndpointId ? `/ai/endpoints/${aiEndpointId}` : "/ai/endpoints",
      waitFor: aiEndpointId
        ? '[class*="ai-endpoint"]'
        : 'h1:has-text("AI endpoints")',
      note: aiEndpointId ? null : "no AI endpoint found — captured list page (empty state)",
    },
    {
      nn: "25", name: "inspector-row-expanded",
      url: insp ?? "/ai/endpoints",
      waitFor: insp ? 'h1:has-text("Inspector")' : 'h1:has-text("AI endpoints")',
      before: async (page) => {
        if (!insp) return;
        // Click the first data row in the inspector table if any
        const rows = page.locator('table[aria-label*="nspect"] tbody tr').filter({ hasNot: page.locator('[class*="skeleton"]') });
        const count = await rows.count();
        if (count > 0) {
          await rows.first().click();
          await page.waitForTimeout(600);
        }
      },
    },
    {
      nn: "26", name: "settings-retention",
      url: "/settings/retention",
      waitFor: 'h1:has-text("Retention")',
    },
    {
      nn: "27", name: "settings-backups",
      url: "/settings/backups",
      waitFor: 'h1:has-text("Backup")',
    },
    {
      nn: "28", name: "settings-openapi",
      url: "/openapi",
      waitFor: 'h1:has-text("OpenAPI")',
    },
    {
      nn: "29", name: "settings-connection-logs",
      url: "/connection-logs",
      waitFor: 'h1:has-text("Connection logs")',
    },
    {
      nn: "30", name: "settings-custom-domains",
      url: "/settings",
      waitFor: 'h1:has-text("Settings")',
      before: async (page) => {
        // Navigate to custom-domains page via the settings card link or directly
        await page.goto(`${BASE_URL}/settings`, { waitUntil: "domcontentloaded" });
        await page.waitForTimeout(800);
        // Look for a "Custom domains" link or card
        const link = page.getByRole("link", { name: /custom domain/i });
        if (await link.count() > 0) {
          await link.first().click();
          await page.waitForTimeout(800);
        }
      },
    },
    {
      nn: "31", name: "settings-smtp",
      url: "/settings",
      waitFor: 'h1:has-text("Settings")',
      before: async (page) => {
        // SMTP section is on /settings page — scroll into view
        const smtpSection = page.locator('text="SMTP"').first();
        if (await smtpSection.count() > 0) {
          await smtpSection.scrollIntoViewIfNeeded();
          await page.waitForTimeout(300);
        }
      },
    },
    {
      nn: "32", name: "audit-row-expanded",
      url: "/audit",
      waitFor: 'h1:has-text("Audit log")',
      before: async (page) => {
        // Click the first clickable audit row
        const rows = page.locator('table tr.clickable');
        const count = await rows.count();
        if (count > 0) {
          await rows.first().click();
          await page.waitForTimeout(500);
        }
      },
    },
    {
      nn: "33", name: "webhook-detail",
      url: "/webhooks",
      waitFor: 'h1:has-text("Webhooks")',
      before: async (page) => {
        // If there are webhook rows, click the ⋯ dropdown on the first to open its menu
        const actionBtns = page.locator('[aria-label^="Actions for"]');
        const count = await actionBtns.count();
        if (count > 0) {
          await actionBtns.first().click();
          await page.waitForTimeout(400);
        }
      },
    },

    // ── Modals ────────────────────────────────────────────────────────────────
    {
      nn: "34", name: "modal-create-token",
      url: "/tokens",
      waitFor: 'h1:has-text("Client tokens")',
      before: async (page) => {
        // The tokens page has an inline form — fill name to enable Create button
        const nameInput = page.locator("#token-name");
        if (await nameInput.count() > 0) {
          await nameInput.fill("screenshot-token");
          await page.waitForTimeout(200);
        }
      },
    },
    {
      nn: "35", name: "modal-create-service",
      url: "/services",
      waitFor: 'h1:has-text("Services")',
      before: async (page) => {
        // Click "New service" button to open dialog
        const btn = page.getByRole("button", { name: /new service/i }).or(
          page.getByRole("button", { name: /\+/ })
        );
        const count = await btn.count();
        if (count > 0) {
          await btn.first().click();
          await page.waitForTimeout(400);
        }
      },
    },
    {
      nn: "36", name: "modal-create-api-key",
      url: svcDetail ?? "/services",
      waitFor: svcDetail ? 'h1:has-text("Service")' : 'h1:has-text("Services")',
      before: async (page) => {
        if (!svcDetail) return;
        // Navigate to API keys tab
        const apiKeysTab = page.getByRole("tab", { name: "API keys" });
        if (await apiKeysTab.count() > 0) {
          await apiKeysTab.click();
          await page.waitForTimeout(300);
          // Click "Create key"
          const createBtn = page.getByRole("button", { name: /create key/i });
          if (await createBtn.count() > 0) {
            await createBtn.first().click();
            await page.waitForTimeout(400);
          }
        }
      },
    },
    {
      nn: "37", name: "modal-token-reveal",
      url: "/tokens",
      waitFor: 'h1:has-text("Client tokens")',
      before: async (page) => {
        // Create a token, then the reveal dialog appears
        const nameInput = page.locator("#token-name");
        if (await nameInput.count() > 0) {
          await nameInput.fill("reveal-probe");
          await page.getByRole("button", { name: "Create", exact: true }).click();
          // Wait for the reveal dialog / token display
          await page.waitForSelector('[role="dialog"], code:has-text("bur_")', { timeout: 8_000 });
          await page.waitForTimeout(400);
        }
      },
    },
    {
      nn: "38", name: "modal-add-webhook",
      url: "/webhooks",
      waitFor: 'h1:has-text("Webhooks")',
      before: async (page) => {
        await page.getByRole("button", { name: "Add webhook" }).click();
        await page.waitForSelector('[role="dialog"]', { timeout: 6_000 });
        await page.waitForTimeout(400);
      },
    },
    {
      nn: "39", name: "modal-replay-request",
      url: insp ?? "/ai/endpoints",
      waitFor: insp ? 'h1:has-text("Inspector")' : 'h1:has-text("AI endpoints")',
      before: async (page) => {
        if (!insp) return;
        // Select first row, then click Replay button
        const rows = page.locator('table[aria-label*="nspect"] tbody tr').filter({ hasNot: page.locator('[class*="skeleton"]') });
        const count = await rows.count();
        if (count > 0) {
          await rows.first().click();
          await page.waitForTimeout(400);
          const replayBtn = page.getByRole("button", { name: /replay/i });
          if (await replayBtn.count() > 0) {
            await replayBtn.first().click();
            await page.waitForSelector('[role="dialog"]', { timeout: 6_000 });
            await page.waitForTimeout(400);
          }
        }
      },
    },
    {
      nn: "40", name: "modal-backup-restore-confirm",
      url: "/settings/backups",
      waitFor: 'h1:has-text("Backup")',
      before: async (page) => {
        // The Restore button only enables after a file is uploaded via the file
        // input. In the e2e lab (no uploaded backup) it is always disabled.
        // Attempt a force-click to open the confirm dialog; if the button is
        // disabled/absent, skip gracefully and capture the empty state.
        const restoreBtn = page.getByRole("button", { name: /restore/i });
        const count = await restoreBtn.count();
        if (count > 0) {
          const disabled = await restoreBtn.first().getAttribute("disabled");
          if (!disabled) {
            await restoreBtn.first().click();
            await page.waitForSelector('[role="dialog"]', { timeout: 6_000 });
            await page.waitForTimeout(400);
            // DO NOT confirm — screenshot open dialog, then it will be cleaned up
          }
          // If disabled (no file uploaded) just screenshot the empty state
        }
      },
    },
  ];
}

// ─── Main ─────────────────────────────────────────────────────────────────────

async function main() {
  const themes = ["light", "dark"];
  const captures = { total: 0, errors: [] };

  // Ensure output dirs
  ensureDir(path.join(OUT_DIR, "light"));
  ensureDir(path.join(OUT_DIR, "dark"));

  const browser = await chromium.launch({ headless: true });
  let aiServiceId = null;
  let aiEndpointId = null;

  try {
    // ── Login once in a temporary context to discover IDs ────────────────────
    {
      const ctx = await browser.newContext({
        viewport: VIEWPORT,
        ignoreHTTPSErrors: true,
      });
      const page = await ctx.newPage();
      await loginOnce(page);
      aiServiceId = await fetchFirstAiServiceId(page);
      aiEndpointId = await fetchFirstAiEndpointId(page);
      console.log(`[info] AI service id: ${aiServiceId ?? "(none)"}`);
      console.log(`[info] AI endpoint id: ${aiEndpointId ?? "(none)"}`);
      await ctx.close();
    }

    const targets = buildTargets(aiServiceId, aiEndpointId);

    for (const theme of themes) {
      const isDark = theme === "dark";
      console.log(`\n── Theme: ${theme} ──────────────────────────────────────`);

      // Each theme gets a fresh browser context with addInitScript for theme
      const ctx = await browser.newContext({
        viewport: VIEWPORT,
        ignoreHTTPSErrors: true,
      });

      // addInitScript: before any page JS runs, set the localStorage key so
      // ThemeProvider picks it up on mount.
      await ctx.addInitScript((dark) => {
        localStorage.setItem("burrow-theme", dark ? "dark" : "light");
      }, isDark);

      const page = await ctx.newPage();

      // Login
      await loginOnce(page);
      // Apply theme after login
      await applyTheme(page, isDark);

      let isLoggedIn = true;

      for (const target of targets) {
        const filename = `${target.nn}-${target.name}.png`;
        const outPath = path.join(OUT_DIR, theme, filename);

        try {
          if (target.loggedOut && isLoggedIn) {
            // Log out for the login page capture
            try {
              await page.evaluate(async (base) => {
                await fetch(`${base}/api/v1/auth/logout`, { method: "POST" });
              }, BASE_URL);
            } catch { /* ignore */ }
            await page.goto(`${BASE_URL}/login`, { waitUntil: "domcontentloaded" });
            await page.waitForTimeout(400);
            await applyTheme(page, isDark);
            await page.waitForTimeout(400);
            isLoggedIn = false;
          } else {
            if (!isLoggedIn) {
              // Re-login after the logged-out capture
              await loginOnce(page);
              await applyTheme(page, isDark);
              isLoggedIn = true;
            }

            await navigateAndSettle(page, target.url, isDark);

            if (target.waitFor) {
              try {
                await page.waitForSelector(target.waitFor, { timeout: 12_000 });
              } catch {
                // Non-fatal: element may not exist; screenshot anyway
              }
            }

            if (target.before) {
              await target.before(page);
              // Re-apply theme after any before() actions (e.g. modal open)
              await applyTheme(page, isDark);
              await page.waitForTimeout(200);
            }
          }

          await page.screenshot({ path: outPath, fullPage: true });
          captures.total++;
          const stat = fs.statSync(outPath);
          const kb = (stat.size / 1024).toFixed(1);
          const note = target.note ? ` [${target.note}]` : "";
          console.log(`  [OK] ${filename} (${kb} KB)${note}`);

        } catch (err) {
          const msg = `${theme}/${filename}: ${err.message ?? String(err)}`;
          captures.errors.push(msg);
          console.error(`  [ERR] ${msg}`);
          // Try to save a screenshot of the broken state anyway
          try {
            await page.screenshot({ path: outPath, fullPage: true });
          } catch { /* ignore */ }
          // After an error, re-navigate to a known-good page + re-login if needed
          try {
            await navigateAndSettle(page, "/tunnels", isDark);
            isLoggedIn = true;
          } catch { /* ignore */ }
        }
      }

      await ctx.close();
    }

  } finally {
    await browser.close();
  }

  // ── Generate index.html gallery ────────────────────────────────────────────
  const targets = buildTargets(aiServiceId, aiEndpointId);
  generateGallery(targets);

  // ── Summary ────────────────────────────────────────────────────────────────
  const total = targets.length * 2;
  console.log(`\n────────────────────────────────────────────────────────────`);
  console.log(`Captures: ${captures.total}/${total}`);
  if (captures.errors.length > 0) {
    console.error(`Errors (${captures.errors.length}):`);
    for (const e of captures.errors) console.error(`  - ${e}`);
    process.exit(1);
  } else {
    console.log("All captures succeeded.");
    console.log(`Gallery: ${path.join(OUT_DIR, "index.html")}`);
  }
}

function generateGallery(targets) {
  const rows = targets.map((t) => {
    const lightSrc = `light/${t.nn}-${t.name}.png`;
    const darkSrc = `dark/${t.nn}-${t.name}.png`;
    return `
    <tr>
      <td class="label">${t.nn}-${t.name}</td>
      <td><a href="${lightSrc}" target="_blank"><img src="${lightSrc}" alt="${t.nn}-${t.name} light" loading="lazy"></a></td>
      <td><a href="${darkSrc}" target="_blank"><img src="${darkSrc}" alt="${t.nn}-${t.name} dark" loading="lazy"></a></td>
    </tr>`;
  }).join("\n");

  const html = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Burrow UI Screenshots</title>
  <style>
    body { font-family: system-ui, sans-serif; background: #0f172a; color: #e2e8f0; margin: 0; padding: 1rem 2rem; }
    h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
    p.meta { color: #94a3b8; font-size: 0.85rem; margin-bottom: 1.5rem; }
    table { border-collapse: collapse; width: 100%; }
    thead th { text-align: left; padding: 0.5rem 0.75rem; background: #1e293b; font-size: 0.8rem; text-transform: uppercase; color: #94a3b8; }
    tbody tr:nth-child(even) td { background: #0f1f35; }
    td { padding: 0.5rem 0.75rem; vertical-align: top; }
    td.label { width: 14rem; font-family: monospace; font-size: 0.85rem; color: #7dd3fc; vertical-align: middle; white-space: nowrap; }
    img { width: 100%; max-width: 820px; border: 1px solid #334155; border-radius: 4px; display: block; }
    a:hover img { border-color: #7dd3fc; }
    .col-header { font-size: 0.9rem; color: #e2e8f0; }
  </style>
</head>
<body>
  <h1>Burrow UI Screenshot Gallery</h1>
  <p class="meta">1920×1080 full-page · light vs dark · generated ${new Date().toISOString()}</p>
  <table>
    <thead>
      <tr>
        <th>Target</th>
        <th class="col-header">Light</th>
        <th class="col-header">Dark</th>
      </tr>
    </thead>
    <tbody>
${rows}
    </tbody>
  </table>
</body>
</html>`;

  fs.writeFileSync(path.join(OUT_DIR, "index.html"), html, "utf8");
}

main().catch((err) => {
  console.error("[FATAL]", err);
  process.exit(1);
});
