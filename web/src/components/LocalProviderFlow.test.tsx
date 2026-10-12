import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { LocalProviderFlow } from "@/components/LocalProviderFlow";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

// The seed already has a provider and a service called ollama; the flow is
// tried on a relay that has neither.
function withoutOllama() {
  db.aiProviders = db.aiProviders.filter((p) => p.slug !== "ollama");
  db.services = db.services.filter((s) => s.name !== "ollama");
}

function mount({ keepSeed = false } = {}) {
  if (!keepSeed) withoutOllama();
  const onDone = vi.fn();
  const onUseExisting = vi.fn();
  const view = renderApp(
    <>
      <Routes>
        <Route path="/gateway/providers" element={<LocalProviderFlow onDone={onDone} onUseExisting={onUseExisting} />} />
        <Route path="*" element={null} />
      </Routes>
      <Where />
    </>,
    "/gateway/providers",
  );
  return { onDone, onUseExisting, ...view };
}

// Every request of the flow, in the order it was made.
function recordRequests() {
  const made: { method: string; path: string; body: Record<string, unknown> | null }[] = [];
  server.events.on("request:start", async ({ request }) => {
    const entry = { method: request.method, path: new URL(request.url).pathname.replace("/api/v1", ""), body: null as Record<string, unknown> | null };
    made.push(entry);
    if (request.method === "POST") entry.body = (await request.clone().json().catch(() => null)) as Record<string, unknown> | null;
  });
  return {
    made,
    posts: () => made.filter((r) => r.method === "POST"),
    stop: () => server.events.removeAllListeners(),
  };
}

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
const lineTexts = () => Array.from(document.querySelectorAll(".install-line pre code")).map((c) => c.textContent);
const runLine = () => screen.getByRole("group", { name: "Run command" });

async function nameIt(name: string) {
  await userEvent.type(await screen.findByLabelText("Name"), name);
}
const proceed = () => userEvent.click(screen.getByRole("button", { name: "Continue" }));

// The ds Select is a button with a listbox.
async function choose(label: string, option: string) {
  await userEvent.click(screen.getByLabelText(label));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

describe("Guided provider — step Name", () => {
  it("creates the service and the provider, then shows the command", async () => {
    const seen = recordRequests();
    try {
      mount();
      await nameIt("Ollama");
      // The address apps will use, made from the name.
      expect(screen.getByLabelText("Name")).toHaveAccessibleDescription(`${window.location.origin}/ai/ollama/v1`);
      await proceed();
      expect(await screen.findByRole("heading", { name: "Run this on the machine" })).toBeInTheDocument();
      expect(runLine()).toHaveTextContent("burrow http 11434 --name ollama");
      const posts = seen.posts();
      expect(posts.map((p) => p.path)).toEqual(["/services", "/ai/providers"]);
      // The client finds its service by name: the service is named as the command names it.
      expect(posts[0]!.body).toEqual({ service_id: "ollama", title: "ollama", access_mode: "api_key" });
      expect(posts[1]!.body).toEqual({ name: "Ollama", kind: "tunnel", service_id: "ollama", gateway_only: true, api_format: "openai" });
    } finally {
      seen.stop();
    }
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "ollama", name: "Ollama", kind: "tunnel", api_format: "openai", service_id: "ollama" });
    expect(db.services.find((s) => s.id === "ollama")).toMatchObject({ name: "ollama", access_mode: "api_key", gateway_only: true });
  });

  it("sends the chosen format", async () => {
    mount();
    await nameIt("Claude box");
    expect(screen.getByLabelText("API format")).toHaveTextContent("OpenAI");
    await choose("API format", "Anthropic");
    await proceed();
    await screen.findByRole("heading", { name: "Run this on the machine" });
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "claude-box", api_format: "anthropic" });
    expect(runLine()).toHaveTextContent("burrow http 11434 --name claude-box");
  });

  it("offers Ollama, LM Studio and another port or address as the model server", async () => {
    mount();
    await nameIt("Studio");
    await userEvent.click(screen.getByLabelText("Model server"));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["Ollama · 11434", "LM Studio · 1234", "Other port or address"]);
    await userEvent.click(screen.getByRole("option", { name: "LM Studio · 1234" }));
    await proceed();
    await screen.findByRole("heading", { name: "Run this on the machine" });
    expect(runLine()).toHaveTextContent("burrow http 1234 --name studio");
  });

  it("takes another address as it is typed", async () => {
    mount();
    await nameIt("vLLM");
    expect(screen.queryByLabelText("Port or address")).toBeNull();
    await choose("Model server", "Other port or address");
    await userEvent.type(screen.getByLabelText("Port or address"), "192.168.1.20:8000");
    await proceed();
    await screen.findByRole("heading", { name: "Run this on the machine" });
    expect(runLine()).toHaveTextContent("burrow http 192.168.1.20:8000 --name vllm");
  });

  it("a taken name is reported on the name field and creates nothing more", async () => {
    const seen = recordRequests();
    try {
      // The seed has a service of that name.
      mount({ keepSeed: true });
      await nameIt("Ollama");
      await proceed();
      const field = screen.getByLabelText("Name");
      await waitFor(() => expect(field).toHaveAttribute("aria-invalid", "true"));
      expect(field).toHaveAccessibleDescription("A service named ollama already exists. Choose another name, or use that service.");
      expect(seen.posts().map((p) => p.path)).toEqual(["/services"]);
      expect(screen.queryByRole("heading", { name: "Run this on the machine" })).toBeNull();
      // Typing takes the server's verdict away.
      await userEvent.type(field, "2");
      expect(field).not.toHaveAttribute("aria-invalid");
    } finally {
      seen.stop();
    }
  });

  it("when the provider cannot be created the service is named and the step can be retried", async () => {
    let fail = true;
    server.use(http.post("/api/v1/ai/providers", () => (fail ? HttpResponse.json({ error: "store is busy" }, { status: 500 }) : undefined)));
    const seen = recordRequests();
    try {
      mount();
      await nameIt("Ollama");
      await proceed();
      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Service ollama was created, but the provider was not: store is busy. Retry creates the provider only.",
      );
      expect(screen.queryByRole("heading", { name: "Run this on the machine" })).toBeNull();
      expect(db.services.some((s) => s.id === "ollama")).toBe(true);
      fail = false;
      await userEvent.click(screen.getByRole("button", { name: "Retry" }));
      await screen.findByRole("heading", { name: "Run this on the machine" });
      // One service, two attempts at the provider.
      expect(seen.posts().map((p) => p.path)).toEqual(["/services", "/ai/providers", "/ai/providers"]);
      expect(runLine()).toHaveTextContent("burrow http 11434 --name ollama");
    } finally {
      seen.stop();
    }
  });

  it("a non-admin is told who can add a provider", async () => {
    db.me.role = "user";
    mount();
    await nameIt("Ollama");
    await proceed();
    expect(await screen.findByRole("alert")).toHaveTextContent("Adding a provider needs an admin.");
    expect(screen.queryByRole("heading", { name: "Run this on the machine" })).toBeNull();
    expect(screen.queryByRole("group", { name: "Run command" })).toBeNull();
    expect(db.services.some((s) => s.id === "ollama")).toBe(false);
  });

  it("Continue is disabled without a name and says why", async () => {
    mount();
    const button = await screen.findByRole("button", { name: "Continue" });
    expect(button).toBeDisabled();
    expect(button).toHaveAccessibleDescription("Enter a name.");
    expect(screen.getByText("Enter a name.")).toBeVisible();
    // Nothing is called wrong before a field was left.
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByLabelText("Name")).not.toHaveAttribute("aria-invalid");
    await nameIt("AI");
    expect(button).toBeDisabled();
    expect(button).toHaveAccessibleDescription("Use a name with at least three letters or digits.");
    await userEvent.type(screen.getByLabelText("Name"), " box");
    expect(button).toBeEnabled();
    expect(button).not.toHaveAccessibleDescription();
  });

  it("says what is wrong with a name once the field is left", async () => {
    mount();
    const field = await screen.findByLabelText("Name");
    await userEvent.type(field, "v1");
    expect(field).not.toHaveAttribute("aria-invalid");
    await userEvent.tab();
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(field).toHaveAccessibleDescription("Use a name with at least three letters or digits.");
  });

  it("an invalid custom address is refused before any request", async () => {
    const seen = recordRequests();
    try {
      mount();
      await nameIt("Ollama");
      await choose("Model server", "Other port or address");
      const address = screen.getByLabelText("Port or address");
      const button = screen.getByRole("button", { name: "Continue" });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription("Enter host:port or a port.");
      expect(address).not.toHaveAttribute("aria-invalid");
      for (const bad of ["localhost", "http://localhost:8000", "8000 ; id", "host:99999", "0"]) {
        await userEvent.clear(address);
        await userEvent.type(address, bad);
        expect(button, bad).toBeDisabled();
      }
      await userEvent.tab();
      expect(address).toHaveAttribute("aria-invalid", "true");
      expect(address).toHaveAccessibleDescription(/host:port or a port/);
      expect(seen.posts()).toEqual([]);
      for (const good of ["8000", "localhost:8000", "[::1]:8000"]) {
        await userEvent.clear(address);
        await userEvent.type(address, good.replace("[", "[["));
        expect(button, good).toBeEnabled();
      }
    } finally {
      seen.stop();
    }
  });

  it("offers a service that already exists instead", async () => {
    const { onUseExisting } = mount();
    await userEvent.click(await screen.findByRole("button", { name: "Use a service that already exists" }));
    expect(onUseExisting).toHaveBeenCalledTimes(1);
  });
});

describe("Guided provider — step Run this on the machine", () => {
  async function toRunStep(name = "Ollama") {
    await nameIt(name);
    await proceed();
    return screen.findByRole("heading", { name: "Run this on the machine" });
  }

  it("shows only the run line when a client is known", async () => {
    statusFeed("ollama", [{ state: "never_connected" }]);
    mount();
    const heading = await toRunStep();
    // The step that was just reached is where the focus is.
    expect(heading).toHaveFocus();
    await waitFor(() => expect(lineTexts()).toEqual(["burrow http 11434 --name ollama"]));
    expect(screen.queryByRole("radiogroup", { name: "Operating system" })).toBeNull();
    // A machine that has no client yet needs the other two.
    const more = screen.getByRole("button", { name: "This is a new machine" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(more);
    expect(more).toHaveAttribute("aria-expanded", "true");
    expect(lineTexts()).toEqual([
      expect.stringMatching(/install\.(sh \| sh|ps1 \| iex)$/),
      expect.stringMatching(/^burrow login /),
      "burrow http 11434 --name ollama",
    ]);
    expect(screen.getByRole("radiogroup", { name: "Operating system" })).toBeInTheDocument();
  });

  it("shows all three lines when no client is known", async () => {
    db.tokens = [];
    db.clients = [];
    statusFeed("ollama", [{ state: "never_connected" }]);
    mount();
    await toRunStep();
    expect(lineTexts()).toHaveLength(3);
    expect(lineTexts()[2]).toBe("burrow http 11434 --name ollama");
    expect(screen.getByRole("radiogroup", { name: "Operating system" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "This is a new machine" })).toBeNull();
  });

  it("shows all three lines on a relay that cannot list its known clients and has none connected", async () => {
    db.clients = [];
    server.use(http.get("/api/v1/clients/known", () => HttpResponse.json({ error: "not found" }, { status: 404 })));
    statusFeed("ollama", [{ state: "never_connected" }]);
    mount();
    await toRunStep();
    expect(lineTexts()).toHaveLength(3);
  });

  it("turns Online when the provider reports it", async () => {
    const feed = statusFeed("ollama", [{ state: "never_connected" }, { state: "never_connected" }, { state: "online", model_count: 3 }]);
    mount();
    await toRunStep();
    const waiting = await screen.findByText("Waiting for the client…");
    const status = waiting.closest("[role=status]")!;
    expect(status).not.toBeNull();
    expect(screen.queryByRole("link", { name: "Create a model" })).toBeNull();
    await waitFor(() => expect(within(status as HTMLElement).getByText("Online")).toBeInTheDocument(), { timeout: 8000 });
    expect(status).toHaveTextContent("3 models found");
    expect(screen.queryByText("Waiting for the client…")).toBeNull();
    expect(feed.asked).toBe(3);
    // Nothing is left to ask.
    await pause(2500);
    expect(feed.asked).toBe(3);
  }, 20000);

  it("is Online at once when the client was already serving it", async () => {
    const feed = statusFeed("ollama", [{ state: "online", model_count: 1 }]);
    mount();
    await toRunStep();
    const online = await screen.findByText("Online");
    expect(online.closest("[role=status]")).toHaveTextContent("1 model found");
    expect(feed.asked).toBe(1);
  });

  it("asks a little longer for the catalog of a provider that came online without one, then stops", async () => {
    const feed = statusFeed("ollama", [{ state: "online" }, { state: "online" }, { state: "online", model_count: 2 }]);
    mount();
    await toRunStep();
    const status = (await screen.findByText("Online")).closest("[role=status]")!;
    expect(status).not.toHaveTextContent(/found/);
    await waitFor(() => expect(status).toHaveTextContent("2 models found"), { timeout: 8000 });
    const asked = feed.asked;
    await pause(2500);
    expect(feed.asked).toBe(asked);
  }, 20000);

  it("tells the list of known clients once when the provider turns online", async () => {
    statusFeed("ollama", [{ state: "never_connected" }, { state: "online", model_count: 1 }]);
    mount();
    await toRunStep();
    await screen.findByText("Waiting for the client…");
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    try {
      await screen.findByText("Online", undefined, { timeout: 6000 });
      await pause(2500);
      expect(asked.filter((p) => p === "/api/v1/clients/known")).toHaveLength(1);
    } finally {
      server.events.removeAllListeners();
    }
  }, 20000);

  it("stops asking when the status cannot be read, and says where it shows", async () => {
    let asked = 0;
    server.use(http.get("/api/v1/ai/providers/ollama", () => { asked++; return HttpResponse.json({ error: "boom" }, { status: 500 }); }));
    mount();
    await toRunStep();
    expect(await screen.findByText("The status could not be read. The provider's page shows it.")).toBeInTheDocument();
    await pause(2500);
    expect(asked).toBe(1);
  }, 15000);

  it("closing while waiting keeps the provider and stops polling", async () => {
    const feed = statusFeed("ollama", [{ state: "never_connected" }]);
    const { onDone, unmount } = mount();
    await toRunStep();
    await waitFor(() => expect(feed.asked).toBeGreaterThanOrEqual(1));
    await userEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    expect(onDone).toHaveBeenCalledTimes(1);
    // Closing does not leave the page.
    expect(screen.getByTestId("where")).toHaveTextContent("/gateway/providers");
    unmount();
    expect(db.aiProviders.some((p) => p.slug === "ollama")).toBe(true);
    const asked = feed.asked;
    await pause(2500);
    expect(feed.asked).toBe(asked);
  }, 15000);

  it("offers the next two steps once online", async () => {
    statusFeed("ollama", [{ state: "online", model_count: 1 }]);
    mount();
    await toRunStep();
    expect(await screen.findByRole("link", { name: "Create a model" })).toHaveAttribute("href", "/gateway/models?new=1&provider=ollama");
    expect(screen.getByRole("link", { name: "Create a gateway key" })).toHaveAttribute("href", "/gateway/keys?new=1&provider=ollama");
  });

  it("a next step closes the dialog on its way out", async () => {
    statusFeed("ollama", [{ state: "online", model_count: 1 }]);
    const { onDone } = mount();
    await toRunStep();
    await userEvent.click(await screen.findByRole("link", { name: "Create a gateway key" }));
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("where")).toHaveTextContent("/gateway/keys?new=1&provider=ollama");
  });

  it("Done closes the dialog and opens the provider's page", async () => {
    statusFeed("ollama", [{ state: "never_connected" }]);
    const { onDone } = mount();
    await toRunStep();
    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("where")).toHaveTextContent("/gateway/providers/ollama");
  });
});
