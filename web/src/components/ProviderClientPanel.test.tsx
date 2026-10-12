import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { ProviderClientPanel } from "@/components/ProviderClientPanel";
import type { AiProvider, KnownClient, Service } from "@/lib/contract";

const DAY = 24 * 3600 * 1000;

function provider(over: Partial<AiProvider> = {}): AiProvider {
  return {
    slug: "ollama", name: "Ollama", kind: "tunnel", api_format: "openai", service_id: "svc_1",
    status: "Offline", client_session_id: "", state: "offline", last_seen: null, last_client: "", model_count: 0,
    ...over,
  } as AiProvider;
}

function service(over: Partial<Service> = {}): Service {
  return {
    id: "svc_1", name: "ollama", type: "http", connected: false, local_addr: "", status: "offline",
    last_seen: null, last_client: "", client_session_id: "", provider_slug: "ollama",
    ...over,
  } as Service;
}

/** What the relay says when the panel asks whether the provider came online, and how often it was asked. */
function relaySays(p: AiProvider) {
  const feed = { asked: 0 };
  server.use(http.get(`/api/v1/ai/providers/${p.slug}`, () => {
    feed.asked++;
    return HttpResponse.json(p);
  }));
  return feed;
}

/** The clients the relay knows, and how often it was asked for them. */
function knownClients(rows: Partial<KnownClient>[]) {
  const feed = { asked: 0 };
  server.use(http.get("/api/v1/clients/known", () => {
    feed.asked++;
    return HttpResponse.json(rows);
  }));
  return feed;
}

function mount(p: AiProvider, s: Service, props: { isAdmin?: boolean; onOnline?: () => void } = {}) {
  return renderApp(
    <ProviderClientPanel provider={p} service={s} isAdmin={props.isAdmin ?? true} onOnline={props.onOnline} />,
    "/gateway/providers/ollama",
  );
}

const command = (name: "Install" | "Sign-in" | "Run") => screen.queryByRole("group", { name: `${name} command` });

describe("ProviderClientPanel", () => {
  it("online: names the client and links to it", async () => {
    const p = provider({ state: "online", status: "Connected", last_client: "koehn-laptop" });
    const feed = relaySays(p);
    mount(p, service({ status: "online", connected: true, client_session_id: "sess_1" }));
    const panel = screen.getByRole("region", { name: "Client" });
    expect(panel).toHaveTextContent("Online · Served by koehn-laptop");
    expect(screen.getByRole("link", { name: "koehn-laptop" })).toHaveAttribute("href", "/clients/sess_1");
    // Nothing to start, and nothing to wait for.
    expect(command("Run")).toBeNull();
    expect(screen.queryByText("Waiting for the client…")).toBeNull();
    expect(screen.queryByRole("link", { name: "Connect another machine" })).toBeNull();
    expect(feed.asked).toBe(0);
  });

  it("online: names the client without a link when the relay names no session", () => {
    const p = provider({ state: "online", status: "Connected", last_client: "koehn-laptop" });
    relaySays(p);
    mount(p, service({ status: "online", connected: true }));
    expect(screen.getByRole("region", { name: "Client" })).toHaveTextContent("Online · Served by koehn-laptop");
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("offline: says since when and shows the command that starts it", async () => {
    const lastSeen = new Date(Date.now() - 3 * DAY).toISOString();
    const p = provider({ state: "offline", last_client: "koehn-laptop", last_seen: lastSeen });
    relaySays(p);
    const known = knownClients([]);
    mount(p, service({ last_seen: lastSeen, last_client: "koehn-laptop" }));
    const line = screen.getByRole("region", { name: "Client" }).querySelector("[role=status]") as HTMLElement;
    expect(line).toHaveTextContent("Offline · last served by koehn-laptop 3 days ago");
    expect(line.querySelector("time")).toHaveAttribute("datetime", lastSeen);
    expect(command("Run")).toHaveTextContent("burrow http 11434 --name ollama");
    // A client served it before: the machine is installed and signed in.
    expect(command("Install")).toBeNull();
    expect(command("Sign-in")).toBeNull();
    expect(screen.getByRole("link", { name: "Connect another machine" })).toHaveAttribute("href", "/clients/connect?provider=ollama");
    expect(await screen.findByText("Waiting for the client…")).toBeInTheDocument();
    // Whether a client is known does not matter here.
    expect(known.asked).toBe(0);
  });

  it("never connected: shows all three lines when no client is known", async () => {
    const p = provider({ state: "never_connected" });
    relaySays(p);
    knownClients([]);
    mount(p, service({ status: "never_connected" }));
    expect(screen.getByRole("region", { name: "Client" }).querySelector("[role=status]")).toHaveTextContent(/^Never connected$/);
    await waitFor(() => expect(command("Install")).toHaveTextContent(/install\.(sh \| sh|ps1 \| iex)$/));
    expect(command("Sign-in")).toHaveTextContent(/^burrow login /);
    expect(command("Run")).toHaveTextContent("burrow http 11434 --name ollama");
    // The three lines are how a machine is connected.
    expect(screen.queryByRole("link", { name: "Connect another machine" })).toBeNull();
  });

  it("never connected: shows only the run line when a client is known", async () => {
    const p = provider({ state: "never_connected" });
    relaySays(p);
    knownClients([{ token_id: "tok_1", token_name: "koehn-laptop", status: "offline", sessions: [], served: [] }]);
    mount(p, service({ status: "never_connected" }));
    await waitFor(() => expect(command("Run")).toHaveTextContent("burrow http 11434 --name ollama"));
    expect(command("Install")).toBeNull();
    expect(command("Sign-in")).toBeNull();
    expect(screen.getByRole("link", { name: "Connect another machine" })).toHaveAttribute("href", "/clients/connect?provider=ollama");
  });

  it("never connected: a reader who may not list clients gets all three lines, and the list is not asked for", async () => {
    const p = provider({ state: "never_connected" });
    relaySays(p);
    const known = knownClients([{ token_id: "tok_1", token_name: "koehn-laptop", status: "offline", sessions: [], served: [] }]);
    mount(p, service({ status: "never_connected" }), { isAdmin: false });
    expect(command("Install")).not.toBeNull();
    expect(command("Run")).toHaveTextContent("burrow http 11434 --name ollama");
    await screen.findByText("Waiting for the client…");
    expect(known.asked).toBe(0);
  });

  it.each([
    ["127.0.0.1:8080", "burrow http 8080 --name ollama"],
    ["192.168.1.5:8000", "burrow http 192.168.1.5:8000 --name ollama"],
    ["", "burrow http 11434 --name ollama"],
    // Not an address: nothing of it reaches the command.
    ["$(reboot):80", "burrow http 11434 --name ollama"],
  ])("uses the port of the service's local address when it is known, else 11434 (%j)", (localAddr, line) => {
    const p = provider({ state: "offline" });
    relaySays(p);
    mount(p, service({ local_addr: localAddr }));
    expect(command("Run")).toHaveTextContent(new RegExp(`^${line.replace(/[.$()]/g, "\\$&")}$`));
  });

  it("names the service in the command, not the provider", () => {
    const p = provider({ slug: "local-models", name: "Local models", state: "offline" });
    relaySays(p);
    mount(p, service({ name: "gpu-box", provider_slug: "local-models" }));
    expect(command("Run")).toHaveTextContent(/^burrow http 11434 --name gpu-box$/);
    expect(screen.getByRole("link", { name: "Connect another machine" })).toHaveAttribute("href", "/clients/connect?provider=local-models");
  });

  it("shows no command for a service whose name cannot stand in one", () => {
    const p = provider({ state: "offline" });
    relaySays(p);
    mount(p, service({ name: "x; reboot" }));
    expect(command("Run")).toBeNull();
    expect(document.body).not.toHaveTextContent("burrow http");
    expect(screen.getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/clients/connect?provider=ollama");
  });

  it("tells the page once when a client starts serving it", async () => {
    const onOnline = vi.fn();
    relaySays(provider({ state: "online", status: "Connected", last_client: "koehn-laptop", model_count: 2 }));
    mount(provider({ state: "offline" }), service(), { onOnline });
    await waitFor(() => expect(onOnline).toHaveBeenCalledTimes(1));
    // The page reads the provider again and hands it down; until then nothing calls a second time.
    await new Promise((r) => setTimeout(r, 150));
    expect(onOnline).toHaveBeenCalledTimes(1);
  });

  it("reads the state of a relay that predates it from the status", () => {
    const p = provider({ state: undefined as unknown as AiProvider["state"], status: "Offline" });
    relaySays(p);
    mount(p, service({ status: undefined as unknown as Service["status"], last_seen: undefined as unknown as null }));
    expect(screen.getByRole("region", { name: "Client" }).querySelector("[role=status]")).toHaveTextContent(/^Offline$/);
    expect(command("Run")).toHaveTextContent("burrow http 11434 --name ollama");
  });
});
