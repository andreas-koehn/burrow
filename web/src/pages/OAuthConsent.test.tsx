import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { ReauthDialog } from "@/components/ReauthDialog";
import type { OAuthRequest } from "@/lib/contract";

const goTo = vi.hoisted(() => vi.fn());
vi.mock("@/lib/navigate", () => ({ goTo }));

function renderAt(route: string) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider><QueryClientProvider client={qc}><MemoryRouter initialEntries={[route]}><App /></MemoryRouter><ReauthDialog /></QueryClientProvider></ThemeProvider>,
  );
}

function req(over: Partial<OAuthRequest> = {}): OAuthRequest {
  return {
    id: "r1", client_id: "https://claude.example/client.json", client_name: "Claude Desktop", client_kind: "metadata_document",
    redirect_uri: "https://claude.example/cb", redirect_host: "claude.example", localhost_redirect: false,
    resource: "https://relay.example.com/mcp/burrow", resource_kind: "burrow", source: "", scopes: ["burrow:read"],
    sources: [], can_grant_tools: true, password_required: false,
    created_at: "2026-10-19T08:00:00Z", expires_at: "2026-10-19T08:10:00Z", ...over,
  };
}
const gateway = (over: Partial<OAuthRequest> = {}) => req({
  resource: "https://relay.example.com/mcp", resource_kind: "gateway", scopes: ["mcp:tools"],
  sources: [{ slug: "github", name: "GitHub" }, { slug: "files", name: "Files" }], ...over,
});

function recordPosts() {
  const posts: { path: string; body: Record<string, unknown> }[] = [];
  server.events.on("request:start", async ({ request }) => {
    if (request.method !== "POST") return;
    posts.push({ path: new URL(request.url).pathname, body: (await request.clone().json().catch(() => ({}))) as Record<string, unknown> });
  });
  return posts;
}
const approve = () => screen.getByRole("button", { name: "Approve" });

beforeEach(() => goTo.mockReset());
afterEach(() => { vi.restoreAllMocks(); server.events.removeAllListeners(); });

describe("Consent page", () => {
  it("shows the app, the address it returns to and what it asks for, outside the dashboard shell", async () => {
    db.oauthRequests = [req()];
    renderAt("/oauth/consent?request=r1");
    expect(await screen.findByRole("heading", { name: /Claude Desktop/ })).toBeInTheDocument();
    expect(screen.getByText("claude.example")).toBeInTheDocument();
    expect(screen.getByText(/Burrow management/)).toBeInTheDocument();
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(screen.queryByText(/only this machine/i)).toBeNull();
  });

  it("warns when the app returns to this machine only", async () => {
    db.oauthRequests = [req({ localhost_redirect: true, redirect_host: "127.0.0.1" })];
    renderAt("/oauth/consent?request=r1");
    expect((await screen.findAllByRole("note")).map((n) => n.textContent).join(" ")).toMatch(/only this machine/i);
  });

  it("shows the address it returns to prominently, as strong text in the page's lead", async () => {
    db.oauthRequests = [req()];
    renderAt("/oauth/consent?request=r1");
    const host = await screen.findByText("claude.example");
    expect(host.tagName).toBe("STRONG");
    expect(host).toHaveClass("consent-host");
    expect(host.closest("p")).toHaveTextContent(/your browser goes to claude\.example/i);
  });

  it("offers no switch to full access when the app asked to read only, and approves read-only", async () => {
    db.oauthRequests = [req()];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    await screen.findByRole("heading", { name: /Claude Desktop/ });
    expect(screen.queryByRole("switch")).toBeNull();
    expect(screen.getByText(/read-only access/i)).toBeInTheDocument();
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalledTimes(1));
    expect(goTo.mock.calls[0][0]).toMatch(/^https:\/\/claude\.example\/cb\?code=abc/);
    expect(posts).toEqual([{ path: "/api/v1/oauth/requests/r1/approve", body: { scopes: ["burrow:read"] } }]);
  });

  it("lets the user narrow a full-access request to read-only", async () => {
    db.oauthRequests = [req({ scopes: ["burrow:write"] })];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    await userEvent.click(await screen.findByRole("switch", { name: /read-only/i }));
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts[0].body).toEqual({ scopes: ["burrow:read"] });
  });

  it("sends one decision on a double click on Approve", async () => {
    db.oauthRequests = [req()];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    await screen.findByRole("heading", { name: /Claude Desktop/ });
    await userEvent.dblClick(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts.filter((p) => p.path.endsWith("/approve"))).toHaveLength(1);
    expect(goTo).toHaveBeenCalledTimes(1);
  });

  it("keeps both buttons disabled while the browser leaves for the app", async () => {
    db.oauthRequests = [req()];
    renderAt("/oauth/consent?request=r1");
    await screen.findByRole("heading", { name: /Claude Desktop/ });
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    // goTo is mocked, so the page stays: nothing may be pressed again.
    await new Promise((r) => setTimeout(r, 50));
    expect(approve()).toBeDisabled();
    expect(screen.getByRole("button", { name: "Deny" })).toBeDisabled();
  });

  it("shows markup in the app's name as text", async () => {
    const evil = '<img src=x onerror="window.__pwned=1"><b>Evil</b>';
    db.oauthRequests = [req({ client_name: evil })];
    const { container } = renderAt("/oauth/consent?request=r1");
    const heading = await screen.findByRole("heading", { name: /wants to connect/ });
    expect(heading.textContent).toBe(`${evil} wants to connect`);
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
  });

  it("grants full access when the read-only switch is off", async () => {
    db.oauthRequests = [req({ scopes: ["burrow:write"] })];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    const ro = await screen.findByRole("switch", { name: /read-only/i });
    expect(ro).not.toBeChecked();
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts[0].body).toEqual({ scopes: ["burrow:write"] });
  });

  it("keeps individually requested permissions as they are", async () => {
    db.oauthRequests = [req({ scopes: ["services:read:own", "tunnels:read:own"] })];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    expect(await screen.findByText("services:read:own")).toBeInTheDocument();
    expect(screen.queryByRole("switch")).toBeNull();
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts[0].body).toEqual({});
  });

  it("at the gateway lists the sources, read-only on, and sends the chosen tools", async () => {
    db.oauthRequests = [gateway()];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    const gh = await screen.findByRole("checkbox", { name: /GitHub/ });
    const files = screen.getByRole("checkbox", { name: /Files/ });
    expect(gh).toBeChecked();
    expect(files).toBeChecked();
    expect(screen.getByRole("switch", { name: /read-only/i })).toBeChecked();
    await userEvent.click(files);
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts[0].body).toEqual({ allowed_tools: ["github__*"], tools_read_only: true });
  });

  it("will not approve a gateway grant with no source chosen", async () => {
    db.oauthRequests = [gateway()];
    renderAt("/oauth/consent?request=r1");
    await userEvent.click(await screen.findByRole("checkbox", { name: /GitHub/ }));
    await userEvent.click(screen.getByRole("checkbox", { name: /Files/ }));
    expect(approve()).toBeDisabled();
  });

  it("says so, and cannot approve, when the account may not give tools", async () => {
    db.oauthRequests = [gateway({ can_grant_tools: false })];
    renderAt("/oauth/consent?request=r1");
    expect(await screen.findByText(/may not give tools/i)).toBeInTheDocument();
    expect(approve()).toBeDisabled();
  });

  it("at one source only offers that source", async () => {
    db.oauthRequests = [gateway({ resource: "https://relay.example.com/mcp/github", resource_kind: "source", source: "github", sources: [{ slug: "github", name: "GitHub" }] })];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    expect(await screen.findByRole("checkbox", { name: /GitHub/ })).toBeChecked();
    await userEvent.click(screen.getByRole("switch", { name: /read-only/i }));
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts[0].body).toEqual({ allowed_tools: ["github__*"], tools_read_only: false });
  });

  it("asks for the password when the relay has no services domain, and sends it", async () => {
    db.oauthRequests = [req({ password_required: true })];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    const pw = await screen.findByLabelText("Your password");
    expect(approve()).toBeDisabled();
    await userEvent.type(pw, "password123");
    await userEvent.click(approve());
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(posts[0].body).toEqual({ scopes: ["burrow:read"], password: "password123" });
  });

  it("shows a wrong password and stays", async () => {
    db.oauthRequests = [req({ password_required: true })];
    renderAt("/oauth/consent?request=r1");
    await userEvent.type(await screen.findByLabelText("Your password"), "nope");
    await userEvent.click(approve());
    expect(await screen.findByRole("alert")).toHaveTextContent(/password is incorrect/i);
    expect(goTo).not.toHaveBeenCalled();
    expect(approve()).toBeEnabled();
  });

  it("does not ask for a password when the relay has a services domain", async () => {
    db.oauthRequests = [req()];
    renderAt("/oauth/consent?request=r1");
    await screen.findByRole("heading", { name: /Claude Desktop/ });
    expect(screen.queryByLabelText("Your password")).toBeNull();
  });

  it("denies and follows the redirect", async () => {
    db.oauthRequests = [req()];
    const posts = recordPosts();
    renderAt("/oauth/consent?request=r1");
    await userEvent.click(await screen.findByRole("button", { name: "Deny" }));
    await waitFor(() => expect(goTo).toHaveBeenCalled());
    expect(goTo.mock.calls[0][0]).toContain("error=access_denied");
    expect(posts[0].path).toBe("/api/v1/oauth/requests/r1/deny");
  });

  it("tells a request that is gone, or none given, to start again from the app", async () => {
    renderAt("/oauth/consent?request=nope");
    expect(await screen.findByText(/not valid or has expired/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("does not look anything up without a request id", async () => {
    renderAt("/oauth/consent");
    expect(await screen.findByText(/not valid or has expired/i)).toBeInTheDocument();
  });

  it("is not usable inside a frame", async () => {
    db.oauthRequests = [req()];
    vi.spyOn(window, "top", "get").mockReturnValue({} as Window);
    renderAt("/oauth/consent?request=r1");
    expect(await screen.findByText(/own browser tab/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("shows an error from the relay and stays pending", async () => {
    db.oauthRequests = [gateway()];
    server.use(http.post("/api/v1/oauth/requests/:id/approve", () => HttpResponse.json({ error: "tool source github is gone" }, { status: 400 })));
    renderAt("/oauth/consent?request=r1");
    await screen.findByRole("checkbox", { name: /GitHub/ });
    await userEvent.click(approve());
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(/github is gone/)).toBeInTheDocument();
    expect(goTo).not.toHaveBeenCalled();
  });
});
