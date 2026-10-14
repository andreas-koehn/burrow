import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { delay, http, HttpResponse } from "msw";
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
    // What turns beside the words is decoration: the words alone are announced.
    expect(status).toHaveTextContent(/^Waiting for the client…$/);
    for (const el of status.querySelectorAll("*")) {
      if (el.textContent === "") expect(el).toHaveAttribute("aria-hidden", "true");
    }
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

  it("Try again asks again after a failure, and the asking goes on", async () => {
    let asked = 0;
    server.use(http.get("/api/v1/ai/providers/ollama", () => {
      asked++;
      if (asked === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json({ slug: "ollama", name: "Ollama", kind: "tunnel", api_format: "openai", service_id: "ollama", status: "Offline", state: asked >= 3 ? "online" : "offline", model_count: 1 });
    }));
    mount();
    await screen.findByText("The status could not be read. The provider's page shows it.");
    // Outside what is announced: a button is not part of the message.
    const again = screen.getByRole("button", { name: "Try again" });
    expect(again.closest("[role=status]")).toBeNull();
    await userEvent.click(again);
    const waiting = await screen.findByText("Waiting for the client…");
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    // The button is gone with the failure: the keyboard is on the line that now waits, not on the page.
    expect(waiting.closest("[role=status]")).toHaveFocus();
    expect(document.body).not.toHaveFocus();
    // Not one answer only: the next one comes by itself.
    await screen.findByText("Online", undefined, { timeout: 6000 });
    expect(asked).toBe(3);
  }, 15000);

  it("quiet: after Try again the line that took the focus stays in sight and gives the answer", async () => {
    let asked = 0;
    server.use(http.get("/api/v1/ai/providers/ollama", async () => {
      asked++;
      if (asked === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      // As over a network: an answer takes a moment, so a question asked again is seen to be out.
      if (asked > 2) await delay(150);
      return HttpResponse.json({ slug: "ollama", name: "Ollama", kind: "tunnel", api_format: "openai", service_id: "ollama", status: "Offline", state: "offline", model_count: 0 });
    }));
    renderApp(<WaitForProvider slug="ollama" quiet><p>NEXT_STEPS</p></WaitForProvider>, "/gateway/providers/ollama");
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    const line = screen.getByRole("status");
    expect(line).toHaveFocus();
    // Asked for, so answered: the line the keyboard is on is not hidden.
    await waitFor(() => expect(line).toHaveTextContent("Still offline."));
    expect(line.closest(".visually-hidden")).toBeNull();
    expect(line).toHaveFocus();
    // The asking goes on every two seconds; what the line says does not change with it.
    const said: string[] = [];
    // Every text that was put into the line, not what it holds by the time the observer runs.
    const seen = new MutationObserver((records) => {
      for (const r of records) {
        // A text node changed in place: what it held before is the one thing that is certain.
        if (r.type === "characterData") said.push(r.oldValue ?? "");
        for (const n of r.addedNodes) said.push(n.textContent ?? "");
      }
    });
    seen.observe(line, { childList: true, subtree: true, characterData: true, characterDataOldValue: true });
    const before = asked;
    await waitFor(() => expect(asked).toBeGreaterThanOrEqual(before + 2), { timeout: 6000 });
    seen.disconnect();
    expect(said.filter((t) => t !== "Still offline.")).toEqual([]);
  }, 15000);

  it("quiet: when the provider comes online after Try again, the focused line says so instead of hiding", async () => {
    let asked = 0;
    server.use(http.get("/api/v1/ai/providers/ollama", () => {
      asked++;
      if (asked === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json({ slug: "ollama", name: "Ollama", kind: "tunnel", api_format: "openai", service_id: "ollama", status: "Offline", state: asked >= 3 ? "online" : "offline", model_count: 1 });
    }));
    renderApp(<WaitForProvider slug="ollama" quiet><p>NEXT_STEPS</p></WaitForProvider>, "/gateway/providers/ollama");
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    const line = screen.getByRole("status");
    await waitFor(() => expect(line).toHaveTextContent("Online"), { timeout: 6000 });
    expect(line.closest(".visually-hidden")).toBeNull();
    expect(line).toHaveFocus();
    expect(screen.getByText("NEXT_STEPS")).toBeInTheDocument();
  }, 15000);

  it("says what it is given when the status cannot be read", async () => {
    server.use(http.get("/api/v1/ai/providers/ollama", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderApp(<WaitForProvider slug="ollama" failureText="The status could not be read." />, "/gateway/providers/ollama");
    expect(await screen.findByText("The status could not be read.")).toBeInTheDocument();
    expect(screen.queryByText(/provider's page shows it/)).toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("quiet: asks without a waiting line, and hands over once the provider is online", async () => {
    const feed = statusFeed("ollama", [{ state: "offline" }, { state: "online", model_count: 3 }]);
    renderApp(<WaitForProvider slug="ollama" quiet><p>NEXT_STEPS</p></WaitForProvider>, "/gateway/providers/ollama");
    await waitFor(() => expect(feed.asked).toBe(1));
    // The line is there for what may arrive in it, and holds nothing: no words, nothing that turns.
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
    expect(await screen.findByText("NEXT_STEPS", undefined, { timeout: 6000 })).toBeInTheDocument();
    // Whoever is handed over to says that it is online: no second line here.
    expect(screen.queryByText("Online")).toBeNull();
    expect(screen.queryByText(/models found/)).toBeNull();
  }, 15000);

  it("quiet: a failure is still said, with Try again", async () => {
    server.use(http.get("/api/v1/ai/providers/ollama", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    renderApp(<WaitForProvider slug="ollama" quiet failureText="The status could not be read." />, "/gateway/providers/ollama");
    const said = await screen.findByText("The status could not be read.");
    expect(said.closest("[role=status]")).not.toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

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
