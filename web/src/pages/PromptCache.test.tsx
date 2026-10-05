import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import PromptCache from "@/pages/PromptCache";

function mount() {
  return renderApp(<PromptCache />, "/gateway/cache");
}

describe("Prompt cache settings (§4.21)", () => {
  it("renders the shared preamble verbatim", async () => {
    mount();
    expect(
      await screen.findByText(
        (_, el) =>
          el?.tagName === "P" &&
          el.textContent === "The cache lives on this relay. No data is sent anywhere.",
      ),
    ).toBeInTheDocument();
  });

  it("Exact tab Save issues PUT /cache/settings + toasts 'Cache settings saved.'", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    // ensure Exact is the active tab
    await userEvent.click(await screen.findByRole("tab", { name: /exact/i }));
    const ttl = await screen.findByLabelText(/ttl/i);
    await userEvent.clear(ttl);
    await userEvent.type(ttl, "900");
    await userEvent.click(screen.getByRole("button", { name: /save cache settings/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/cache/settings")
          && (init as RequestInit | undefined)?.method === "PUT",
        ),
      ).toBe(true);
    });
    expect((await screen.findAllByText(/cache settings saved/i)).length).toBeGreaterThan(0);
  });

  it("Clear cache prompts a destructive confirm and then DELETEs /cache/entries", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /clear cache/i }));
    expect(
      await screen.findByText("Clear all cached responses? This cannot be undone."),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^clear$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          String(url).endsWith("/api/v1/cache/entries")
          && (init as RequestInit | undefined)?.method === "DELETE",
        ),
      ).toBe(true);
    });
    expect((await screen.findAllByText(/cache cleared/i)).length).toBeGreaterThan(0);
  });

  // v0.5.0 promoted Semantic tab tests

  it("Semantic tab Switch is enabled (no aria-disabled)", async () => {
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    const sw = await screen.findByRole("switch", { name: /enable semantic cache/i });
    expect(sw).not.toHaveAttribute("aria-disabled", "true");
    expect(sw).not.toBeDisabled();
  });

  it("Semantic tab v0.5 preview badge is gone", async () => {
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    await screen.findByRole("switch", { name: /enable semantic cache/i });
    expect(screen.queryByText(/v0\.5 preview/i)).not.toBeInTheDocument();
  });

  it("banner verbatim text is present on the Semantic tab", async () => {
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    expect(
      await screen.findByText(
        "Semantic caching is off by default. Enable only after reading the docs — vector similarity can return stale or near-miss answers.",
      ),
    ).toBeInTheDocument();
  });

  it("Flipping Semantic Switch reveals seven v0.5.0 fields", async () => {
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    const sw = await screen.findByRole("switch", { name: /enable semantic cache/i });
    await userEvent.click(sw);
    // seven fields
    expect(await screen.findByLabelText(/min.*similarity/i)).toBeInTheDocument();
    expect(await screen.findByText(/^embedding mode$/i)).toBeInTheDocument();
    expect(await screen.findByLabelText(/embedding.*url/i)).toBeInTheDocument();
    expect(await screen.findByText(/^embedding model$/i)).toBeInTheDocument();
    expect(await screen.findByText(/^fallback policy$/i)).toBeInTheDocument();
    expect(await screen.findByRole("switch", { name: /promote.*on.*miss/i })).toBeInTheDocument();
    expect(await screen.findByLabelText(/max.*index.*entries/i)).toBeInTheDocument();
  });

  it("Out-of-range min_similarity shows inline alert and disables Save", async () => {
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    const sw = await screen.findByRole("switch", { name: /enable semantic cache/i });
    await userEvent.click(sw);
    const simInput = await screen.findByLabelText(/min.*similarity/i);
    await userEvent.clear(simInput);
    await userEvent.type(simInput, "1.5");
    expect(
      await screen.findByText("Similarity must be between 0 and 1."),
    ).toBeInTheDocument();
    const saveBtn = screen.getByRole("button", { name: /save semantic/i });
    expect(saveBtn).toBeDisabled();
  });

  it("Semantic Save → PUT /services/{id}/ai-config + toast 'Semantic cache settings saved.'", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    const sw = await screen.findByRole("switch", { name: /enable semantic cache/i });
    await userEvent.click(sw);
    await userEvent.click(screen.getByRole("button", { name: /save semantic/i }));
    await waitFor(() => {
      const call = fetchSpy.mock.calls.find(([url, init]) =>
        String(url).endsWith("/api/v1/services/svc_ai001/ai-config")
        && (init as RequestInit | undefined)?.method === "PUT",
      );
      expect(call).toBeDefined();
      if (call) {
        const reqBody = JSON.parse((call[1] as RequestInit).body as string) as {
          cache?: { semantic?: { enabled?: boolean } };
        };
        expect(reqBody.cache?.semantic?.enabled).toBe(true);
      }
    });
    expect((await screen.findAllByText(/semantic cache settings saved/i)).length).toBeGreaterThan(0);
  });

  it("Semantic stats panel shows the five v0.5.0 semantic_* fields", async () => {
    mount();
    await userEvent.click(await screen.findByRole("tab", { name: /semantic/i }));
    // Stats labels
    expect(await screen.findByText(/semantic entries/i)).toBeInTheDocument();
    expect(await screen.findByText(/semantic disk/i)).toBeInTheDocument();
    expect(await screen.findByText(/semantic hit rate/i)).toBeInTheDocument();
    expect(await screen.findByText(/similar returned/i)).toBeInTheDocument();
    expect(await screen.findByText(/promotions/i)).toBeInTheDocument();
  });
});
