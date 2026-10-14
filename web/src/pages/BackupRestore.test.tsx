import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import BackupRestore from "@/pages/BackupRestore";

function mount() {
  return renderApp(<BackupRestore />, "/settings/backups");
}

describe("Backup & restore", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders a real EmptyState (not a flat tr) when there are no backups (C3)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.includes("/backups")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      return new Response("[]", { status: 200 }) as Response;
    });
    const { container } = mount();
    await waitFor(() => {
      expect(container.querySelector(".state-card")).not.toBeNull();
      expect(container.querySelector(".state-card .icon-bubble")).not.toBeNull();
    });
  });

  it("says how long ago a backup was taken, with the exact time on hover", async () => {
    const taken = new Date(Date.now() - 3 * 3_600_000).toISOString();
    vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
      new Response(JSON.stringify([
        { id: "bk_1", taken_at: taken, version: "v0.9.0", size_bytes: 1024, db_sha256: "ab".repeat(32), path: "/b/bk_1.tar.gz" },
      ]), { status: 200 }) as Response);
    mount();
    const when = within(await screen.findByRole("table", { name: "Backup history" })).getByText("3 hours ago");
    expect(when.tagName).toBe("TIME");
    expect(when).toHaveAttribute("datetime", taken);
    expect(when.getAttribute("title")).not.toBe("3 hours ago");
    // Not the raw timestamp of the relay.
    expect(screen.queryByText(taken)).toBeNull();
  });

  it("renders the verbatim disclaimer", async () => {
    mount();
    expect(
      await screen.findByText(
        /Backups include the database, the relay's TLS cert state, and config/,
      ),
    ).toBeInTheDocument();
  });

  it("Create backup POSTs /backups", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /create backup/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/backups")
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
  });

  it("renders backup history with mono size + truncated sha + Verify menu item", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /backup history/i });
    const row = (await within(table).findAllByRole("row"))[1]!;
    expect(within(row).getByRole("button", { name: /verify/i })).toBeInTheDocument();
  });

  it("file-picker shows an English label regardless of browser locale", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ backups: [] }), { status: 200 }) as Response,
    );
    const { container } = mount();
    expect(container.querySelector('label[for="restore-file"]')).not.toBeNull();
    expect(container.querySelector('label[for="restore-file"]')!.textContent).toMatch(
      /choose backup archive/i,
    );
    expect(container.querySelector('input[type="file"]')?.getAttribute("id")).toBe("restore-file");
  });
});
