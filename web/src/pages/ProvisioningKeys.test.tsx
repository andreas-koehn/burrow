import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import ProvisioningKeys from "@/pages/ProvisioningKeys";

function mount() {
  return renderApp(<ProvisioningKeys />, "/provisioning");
}

describe("Provisioning keys (§4.28)", () => {
  it("renders Active provisioning keys + Pending approvals sections", async () => {
    mount();
    expect(await screen.findByRole("table", { name: /active provisioning keys/i })).toBeInTheDocument();
    expect(screen.getByRole("table", { name: /pending approvals/i })).toBeInTheDocument();
  });

  it("New provisioning key reveals the plaintext once with the install snippet", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New provisioning key" }));
    const dlg = await screen.findByRole("dialog", { name: "New provisioning key" });
    await userEvent.type(within(dlg).getByLabelText(/^name$/i), "fleet-key");
    await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/provisioning/keys")
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
    expect(await screen.findByText(/burrow connect --server/i)).toBeInTheDocument();
  });

  it("Approve flips a pending row into an Active client (mutation)", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    const pending = await screen.findByRole("table", { name: /pending approvals/i });
    const row = within(pending).getAllByRole("row")[1]!;
    await userEvent.click(within(row).getByRole("button", { name: /approve/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          /\/api\/v1\/provisioning\/pending\/[^/]+\/approve$/.test(String(url))
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
  });
});
