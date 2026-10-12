import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { KNOWN_CLIENTS_KEY } from "@/lib/useKnownClients";
import { WaitForProvider } from "@/components/WaitForProvider";

// What GET /ai/providers/<slug> says about the provider's state, one answer after the other.
function statusFeed(slug: string, answers: { state: string; model_count?: number }[]) {
  const feed = { asked: 0 };
  server.use(http.get(`/api/v1/ai/providers/${slug}`, () => {
    const a = answers[Math.min(feed.asked, answers.length - 1)]!;
    feed.asked++;
    return HttpResponse.json({ slug, name: "Ollama", kind: "tunnel", api_format: "openai", service_id: slug, status: a.state === "online" ? "Connected" : "Offline", state: a.state, model_count: a.model_count ?? 0 });
  }));
  return feed;
}

const pause = (ms: number) => new Promise((r) => setTimeout(r, ms));

function mount() {
  return renderApp(<WaitForProvider slug="ollama"><p>NEXT_STEPS</p></WaitForProvider>, "/gateway/providers");
}

describe("WaitForProvider", () => {
  it("waits with an accessible status and turns Online when the provider reports it", async () => {
    const feed = statusFeed("ollama", [{ state: "never_connected" }, { state: "offline" }, { state: "online", model_count: 3 }]);
    mount();
    const waiting = await screen.findByText("Waiting for the client…");
    const status = waiting.closest("[role=status]") as HTMLElement;
    expect(status).not.toBeNull();
    expect(status.querySelector(".spinner")).toHaveAttribute("aria-hidden", "true");
    // What comes next is not offered before there is something to use.
    expect(screen.queryByText("NEXT_STEPS")).toBeNull();
    await waitFor(() => expect(within(status).getByText("Online")).toBeInTheDocument(), { timeout: 8000 });
    expect(status).toHaveTextContent("3 models found");
    expect(screen.queryByText("Waiting for the client…")).toBeNull();
    // Below the line, not inside what is announced.
    expect(status).not.toContainElement(screen.getByText("NEXT_STEPS"));
    expect(feed.asked).toBe(3);
    // Nothing is left to ask.
    await pause(2500);
    expect(feed.asked).toBe(3);
  }, 20000);

  it("is Online at once when the client was already serving it", async () => {
    const feed = statusFeed("ollama", [{ state: "online", model_count: 1 }]);
    mount();
    const online = await screen.findByText("Online");
    expect(online.closest("[role=status]")).toHaveTextContent("1 model found");
    expect(screen.getByText("NEXT_STEPS")).toBeInTheDocument();
    expect(feed.asked).toBe(1);
  });

  it("asks a little longer for the catalog of a provider that came online without one, then stops", async () => {
    const feed = statusFeed("ollama", [{ state: "online" }, { state: "online" }, { state: "online", model_count: 2 }]);
    mount();
    const status = (await screen.findByText("Online")).closest("[role=status]")!;
    expect(status).not.toHaveTextContent(/found/);
    await waitFor(() => expect(status).toHaveTextContent("2 models found"), { timeout: 8000 });
    const asked = feed.asked;
    await pause(2500);
    expect(feed.asked).toBe(asked);
  }, 20000);

  it("tells the lists behind it once when the provider turns online", async () => {
    statusFeed("ollama", [{ state: "never_connected" }, { state: "online", model_count: 1 }]);
    const { qc } = mount();
    const told: string[] = [];
    const invalidate = qc.invalidateQueries.bind(qc);
    qc.invalidateQueries = ((filters?: { queryKey?: readonly unknown[] }) => {
      told.push(JSON.stringify(filters?.queryKey));
      return invalidate(filters);
    }) as typeof qc.invalidateQueries;
    await screen.findByText("Waiting for the client…");
    expect(told).toEqual([]);
    await screen.findByText("Online", undefined, { timeout: 6000 });
    await pause(2500);
    expect(told).toEqual([JSON.stringify(KNOWN_CLIENTS_KEY), JSON.stringify(["ai", "providers"])]);
  }, 20000);

  it("stops asking when the status cannot be read, and says where it shows", async () => {
    let asked = 0;
    server.use(http.get("/api/v1/ai/providers/ollama", () => { asked++; return HttpResponse.json({ error: "boom" }, { status: 500 }); }));
    mount();
    expect(await screen.findByText("The status could not be read. The provider's page shows it.")).toBeInTheDocument();
    await pause(2500);
    expect(asked).toBe(1);
  }, 15000);

  it("stops asking when it is taken off the page", async () => {
    const feed = statusFeed("ollama", [{ state: "never_connected" }]);
    const { unmount } = mount();
    await waitFor(() => expect(feed.asked).toBeGreaterThanOrEqual(1));
    unmount();
    const asked = feed.asked;
    await pause(2500);
    expect(feed.asked).toBe(asked);
  }, 15000);
});
