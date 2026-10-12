import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import AutomationTokens from "@/pages/AutomationTokens";

function mount() {
  return renderApp(<AutomationTokens />, "/settings/automation");
}

describe("Automation tokens", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders a real EmptyState (not a flat tr) when there are no tokens (C3)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.includes("/automation/tokens")) {
        return new Response(JSON.stringify([]), { status: 200 }) as Response;
      }
      return new Response(JSON.stringify([]), { status: 200 }) as Response;
    });
    const { container } = mount();
    await waitFor(() => {
      expect(container.querySelector(".state-card")).not.toBeNull();
      expect(container.querySelector(".state-card .icon-bubble")).not.toBeNull();
    });
    const card = container.querySelector(".state-card")!;
    expect(card.textContent).not.toContain("Long-lived bearer tokens for CI / CLI / bots");
    expect(card.textContent).toMatch(/mint a token/i);
  });

  it("renders a row per token with mono bua_ prefix", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /automation tokens/i });
    expect(within(table).getByText(/bua_/)).toBeInTheDocument();
  });

  // P4.3 — reciprocal hint
  it("says these tokens are for scripts, not clients (P4.3)", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }) as Response);
    mount();
    const hint = screen.getByText(/These authenticate scripts, not clients\./);
    expect(hint).toHaveTextContent(
      "These authenticate scripts, not clients. To connect a machine that runs burrow, use Client tokens. Apps that call a model use a gateway key.",
    );
  });

  it("points apps that call a model to gateway keys", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }) as Response);
    mount();
    expect(screen.getByRole("link", { name: "gateway key" })).toHaveAttribute("href", "/gateway/keys");
  });

  it("renders Client tokens cross-link pointing to the Tokens tab of Clients (P4.3)", () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify([]), { status: 200 }) as Response);
    mount();
    const link = screen.getByRole("link", { name: /client tokens/i });
    expect(link).toHaveAttribute("href", "/clients?tab=tokens");
  });

  it("Creating a token reveals the plaintext once with the verbatim save-now warning", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /new token/i }));
    const dlg = await screen.findByRole("dialog");
    await userEvent.type(within(dlg).getByLabelText(/^name$/i), "ci-bot");
    await userEvent.click(within(dlg).getByRole("button", { name: /^create$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/automation/tokens")
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
    expect(
      await screen.findByText("Save this token now — you won't see it again."),
    ).toBeInTheDocument();
  });
});
