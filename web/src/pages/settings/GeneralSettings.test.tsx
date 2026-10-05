import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import GeneralSettings from "@/pages/settings/GeneralSettings";

describe("Settings / General page", () => {
  it("is headed General and says what it covers", async () => {
    renderApp(<GeneralSettings />);
    expect(await screen.findByRole("heading", { name: "General", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Settings that apply to the whole relay.")).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { name: /^general$/i })).toHaveLength(1);
  });

  // The card grid is gone: the Settings sidebar is the way to these pages now.
  it("has no card grid and no SMTP form", async () => {
    renderApp(<GeneralSettings />);
    await screen.findByRole("checkbox", { name: /include top source ips/i });
    for (const name of [/Retention & compliance/, /Database backend/, /Backup & restore/, /OpenAPI viewer/, /Connection logs/]) {
      expect(screen.queryByRole("link", { name })).toBeNull();
    }
    expect(screen.queryByRole("heading", { name: /configuration/i })).toBeNull();
    expect(screen.queryByLabelText(/SMTP server/i)).toBeNull();
  });
});

describe("Settings / General / Privacy — connection_logs.rollup_include_top_ips toggle (v0.5.1 Q12)", () => {
  afterEach(() => resetDb());

  it("toggle defaults to ON when the backend has no setting (default-true policy)", async () => {
    // db.settings does NOT carry the key by default.
    delete db.settings["connection_logs.rollup_include_top_ips"];
    renderApp(<GeneralSettings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    expect(toggle).toBeChecked();
  });

  it("toggle reflects the persisted backend value 'false' on load", async () => {
    db.settings["connection_logs.rollup_include_top_ips"] = "false";
    renderApp(<GeneralSettings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    await waitFor(() => expect(toggle).not.toBeChecked());
  });

  it("flipping the toggle issues PUT /settings with the correct key/value (true -> false)", async () => {
    delete db.settings["connection_logs.rollup_include_top_ips"];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    renderApp(<GeneralSettings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);
    await waitFor(() => {
      const sawPut = fetchSpy.mock.calls.some(([url, init]) => {
        const u = String(url);
        const method = (init as RequestInit | undefined)?.method ?? "GET";
        if (method !== "PUT" || !u.includes("/api/v1/settings")) return false;
        const body = (init as RequestInit | undefined)?.body;
        if (typeof body !== "string") return false;
        try {
          const parsed = JSON.parse(body) as Record<string, string>;
          return parsed["connection_logs.rollup_include_top_ips"] === "false";
        } catch {
          return false;
        }
      });
      expect(sawPut).toBe(true);
    });
    // Backend roundtrip — the MSW handler must whitelist the key.
    await waitFor(() => {
      expect(db.settings["connection_logs.rollup_include_top_ips"]).toBe("false");
    });
  });

  // Other pages read the same ["settings"] cache (the Email form, the overview's notice):
  // a one-key map there would look like a relay with nothing configured.
  it("does not seed the shared settings cache when the settings could not be read", async () => {
    server.use(http.get("/api/v1/settings", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    const { qc } = renderApp(<GeneralSettings />);
    const toggle = await screen.findByRole("checkbox", { name: /include top source ips/i });
    await waitFor(() => expect(qc.getQueryState(["settings"])?.status).toBe("error"));
    await userEvent.click(toggle);
    await waitFor(() => expect(db.settings["connection_logs.rollup_include_top_ips"]).toBe("false"));
    await waitFor(() => expect(qc.isMutating()).toBe(0));
    expect(qc.getQueryData(["settings"])).toBeUndefined();
  });
});
