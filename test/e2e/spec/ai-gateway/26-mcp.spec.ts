// test-only — never deploy this shape.
//
// Spec 26 — MCP tool inventory.
// API coverage of /api/v1/mcp/tools. The dashboard has no MCP page yet; when one
// is added, its UI check belongs here.

import { test, expect } from "@playwright/test";
import { AUTH_STORAGE_PATH } from "../../fixtures/auth";

test.use({ storageState: AUTH_STORAGE_PATH });

test("26-mcp: tool inventory — API returns ≥10 tools", async ({ request }) => {
  // 1. API: backend must expose ≥10 MCP tools.
  const apiResp = await request.get("/api/v1/mcp/tools");
  expect(apiResp.status(), "GET /api/v1/mcp/tools").toBe(200);
  const tools = (await apiResp.json()) as unknown[];
  expect(Array.isArray(tools), "tools is array").toBe(true);
  expect(tools.length, `tool count (${tools.length}) ≥ 10`).toBeGreaterThanOrEqual(10);
});
