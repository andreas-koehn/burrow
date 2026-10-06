import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { ThemeProvider } from "@/components/theme-provider";
import App from "@/App";
import { setCsrfCookie } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";

const CODE = "BRRW-7Q4K";
const INVALID = "This code is not valid or has expired. Run burrow login again.";
const REQUESTS = "/api/v1/client/login/requests/:code";

function PathProbe() {
  const { pathname, search } = useLocation();
  return <div data-testid="path">{pathname + search}</div>;
}

// The whole app, so that the test sees what is around the page (nothing: it is outside the shell).
function renderAt(route: string) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider><QueryClientProvider client={qc}><MemoryRouter initialEntries={[route]}><App /><PathProbe /></MemoryRouter></QueryClientProvider></ThemeProvider>,
  );
}

// Records every request to the three request endpoints, then lets the mock relay answer.
function recordCalls() {
  const calls: { method: string; path: string; body: string; csrf: string | null }[] = [];
  const spy = async ({ request }: { request: Request }) => {
    calls.push({
      method: request.method, path: new URL(request.url).pathname,
      body: await request.clone().text(), csrf: request.headers.get("X-CSRF-Token"),
    });
    return undefined; // fall through to the handler of the mock relay
  };
  server.use(http.get(REQUESTS, spy), http.post(`${REQUESTS}/approve`, spy), http.post(`${REQUESTS}/deny`, spy));
  return calls;
}

const approve = () => screen.getByRole("button", { name: "Approve" });
const deny = () => screen.getByRole("button", { name: "Deny" });
const request = () => db.clientLogins[0];

afterEach(() => vi.restoreAllMocks());

describe("Link page: a pending request", () => {
  it("shows the code to compare, what the machine says about itself and what the relay saw", async () => {
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByText(CODE)).toBeInTheDocument();
    expect(screen.getByText("Check that your terminal shows the same code.")).toBeInTheDocument();

    const reported = screen.getByRole("group", { name: "Reported by the client" });
    expect(within(reported).getByText("kohns-laptop")).toBeInTheDocument();
    expect(within(reported).getByText("linux")).toBeInTheDocument();
    expect(within(reported).getByText("amd64")).toBeInTheDocument();
    const seen = screen.getByRole("group", { name: "Seen by the relay" });
    expect(within(seen).getByText("203.0.113.24")).toBeInTheDocument();
    expect(within(seen).getByText("1 minute ago")).toBeInTheDocument();

    expect(screen.getByLabelText("Token name")).toHaveValue("kohns-laptop");
    expect(approve()).toBeEnabled();
    expect(deny()).toBeEnabled();
  });

  it("says in plain words what approving does, and for whom", async () => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    const warning = screen.getByRole("note");
    expect(warning).toHaveTextContent("Approving gives that machine access to this relay as you (alice@acme.io).");
    expect(warning).toHaveTextContent("If you did not just run burrow login, deny.");
  });

  it("is outside the workspace shell: no navigation, no workspace switcher", async () => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(screen.queryByRole("link", { name: /^Clients/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /workspace/i })).toBeNull();
  });

  it("approves nothing by itself: opening the page only looks the request up", async () => {
    const calls = recordCalls();
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.map((c) => c.method)).toEqual(["GET"]);
    expect(request().status).toBe("pending");
  });

  it("does not approve on Enter in the token name field", async () => {
    const calls = recordCalls();
    renderAt(`/link?code=${CODE}`);
    await userEvent.type(await screen.findByLabelText("Token name"), "{Enter}");
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.filter((c) => c.method === "POST")).toEqual([]);
    expect(request().status).toBe("pending");
  });

  it("does not put the focus on a button", async () => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    expect(document.activeElement?.tagName).not.toBe("BUTTON");
  });

  it("renders a hostile hostname as text and keeps it bounded", async () => {
    request().hostname = `<img src=x onerror=alert(1)>${"a".repeat(600)}`;
    request().suggested_token_name = `"><script>alert(2)</script>`;
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("script")).toBeNull();
    const reported = screen.getByRole("group", { name: "Reported by the client" });
    const shown = within(reported).getByText(/<img src=x onerror=alert\(1\)>/);
    expect(shown.textContent!.length).toBeLessThanOrEqual(130);
    expect(shown.textContent!.endsWith("…")).toBe(true);
    // Nothing from the client ends up in an attribute that is not a form value.
    for (const el of document.querySelectorAll("[title], [href], [style]")) {
      for (const attr of ["title", "href", "style"]) expect(el.getAttribute(attr) ?? "").not.toMatch(/img|script/);
    }
    expect(screen.getByLabelText("Token name")).toHaveValue(`"><script>alert(2)</script>`);
  });
});

describe("Link page: approve and deny", () => {
  it("approves with the token name and says the terminal continues", async () => {
    const calls = recordCalls();
    renderAt(`/link?code=${CODE}`);
    const name = await screen.findByLabelText("Token name");
    await userEvent.clear(name);
    await userEvent.type(name, "  build box  ");
    await userEvent.click(approve());

    const done = await screen.findByRole("status");
    expect(done).toHaveTextContent("Approved. Your terminal will continue by itself. You can close this page.");
    expect(done).toHaveFocus();
    expect(screen.getByRole("link", { name: "See your clients" })).toHaveAttribute("href", "/clients");
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();

    const posts = calls.filter((c) => c.method === "POST");
    expect(posts).toHaveLength(1);
    expect(posts[0].path).toBe("/api/v1/client/login/requests/BRRW7Q4K/approve");
    expect(JSON.parse(posts[0].body)).toEqual({ token_name: "build box" });
    expect(posts[0].csrf).toBe(db.csrf);
    expect(request().status).toBe("approved");
    // The request is not looked up again once it is decided.
    expect(calls.filter((c) => c.method === "GET")).toHaveLength(1);
  });

  it("denies and says the request was discarded", async () => {
    const calls = recordCalls();
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    await userEvent.click(deny());

    const done = await screen.findByRole("status");
    expect(done).toHaveTextContent("Denied. The sign-in request was discarded.");
    expect(done).toHaveFocus();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    const posts = calls.filter((c) => c.method === "POST");
    expect(posts.map((c) => c.path)).toEqual(["/api/v1/client/login/requests/BRRW7Q4K/deny"]);
    expect(posts[0].csrf).toBe(db.csrf);
    expect(request().status).toBe("denied");
  });

  it("sends one request for a double click and disables both buttons meanwhile", async () => {
    let release = () => {};
    const held = new Promise<void>((r) => { release = r; });
    let posts = 0;
    server.use(http.post(`${REQUESTS}/approve`, async () => { posts++; await held; return undefined; }));
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    await userEvent.dblClick(approve());
    await waitFor(() => expect(approve()).toBeDisabled());
    expect(deny()).toBeDisabled();
    release();
    await screen.findByRole("status");
    expect(posts).toBe(1);
  });

  it("gives Deny the same weight as Approve", async () => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    expect(approve().parentElement).toBe(deny().parentElement);
    expect(approve()).toHaveAttribute("type", "button");
    expect(deny()).toHaveAttribute("type", "button");
    expect(deny().className).toContain("btn");
  });

  it("disables Approve for an empty token name and names the limit for a long one", async () => {
    renderAt(`/link?code=${CODE}`);
    const name = await screen.findByLabelText("Token name");
    await userEvent.clear(name);
    expect(approve()).toBeDisabled();
    expect(deny()).toBeEnabled();
    await userEvent.type(name, "   ");
    expect(approve()).toBeDisabled();

    await userEvent.clear(name);
    await userEvent.click(name);
    await userEvent.paste("x".repeat(121));
    expect(screen.getByText("At most 120 characters.")).toBeInTheDocument();
    expect(approve()).toBeDisabled();
    await userEvent.clear(name);
    await userEvent.click(name);
    await userEvent.paste("x".repeat(120));
    expect(screen.queryByText("At most 120 characters.")).toBeNull();
    expect(approve()).toBeEnabled();
  });

  it("shows the relay's own words when it refuses the token name", async () => {
    renderAt(`/link?code=${CODE}`);
    const name = await screen.findByLabelText("Token name");
    await userEvent.clear(name);
    await userEvent.click(name);
    await userEvent.paste("a​b"); // a format character: the relay refuses it
    await userEvent.click(approve());
    expect(await screen.findByText("token_name must be 1 to 120 characters without control characters")).toBeInTheDocument();
    // Still decidable: the name can be corrected.
    expect(approve()).toBeInTheDocument();
    expect(request().status).toBe("pending");
  });

  it("tells an account without the permission so, and offers nothing to retry", async () => {
    db.me.role = "user";
    db.rolePerms.user = [];
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    await userEvent.click(approve());
    expect(await screen.findByRole("alert")).toHaveTextContent("Your account may not sign machines in.");
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
    expect(request().status).toBe("pending");
  });

  it.each([
    ["approved or denied elsewhere (409)", () => { request().status = "denied"; }],
    ["expired meanwhile (404)", () => { request().expires_at = new Date(Date.now() - 1000).toISOString(); }],
  ])("shows the expired message when the request was %s", async (_name, change) => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    change();
    await userEvent.click(approve());
    const gone = await screen.findByRole("alert");
    expect(gone).toHaveTextContent(INVALID);
    expect(gone).toHaveFocus();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("shows the wrong-code limit as such", async () => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    db.clientLoginWrongCodes = 20;
    await userEvent.click(deny());
    expect(await screen.findByRole("alert")).toHaveTextContent("Too many wrong codes. Wait a minute, then try again.");
  });
});

describe("Link page: codes that lead nowhere", () => {
  it.each([
    ["unknown", "/link?code=ZZZZ-ZZZZ"],
    ["malformed", "/link?code=..%2F..%2Fme"],
  ])("an %s code gets the message and no form", async (_name, route) => {
    renderAt(route);
    expect(await screen.findByRole("alert")).toHaveTextContent(INVALID);
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
    expect(screen.queryByLabelText("Token name")).toBeNull();
    expect(screen.getByRole("link", { name: "Enter another code" })).toHaveAttribute("href", "/link");
  });

  it("never sends a malformed code to the relay", async () => {
    const calls = recordCalls();
    renderAt("/link?code=..%2F..%2Fme");
    await screen.findByRole("alert");
    expect(calls).toEqual([]);
    expect(db.clientLoginWrongCodes).toBe(0);
  });

  it("an expired code gets the same message", async () => {
    request().expires_at = new Date(Date.now() - 1000).toISOString();
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByRole("alert")).toHaveTextContent(INVALID);
  });

  it.each(["approved", "denied"] as const)("a code that was already %s reveals nothing more", async (status) => {
    request().status = status;
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByRole("alert")).toHaveTextContent(INVALID);
    expect(screen.queryByText("kohns-laptop")).toBeNull();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("shows the wrong-code limit on lookup", async () => {
    db.clientLoginWrongCodes = 20;
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByRole("alert")).toHaveTextContent("Too many wrong codes. Wait a minute, then try again.");
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("offers a retry when the relay cannot be asked", async () => {
    let fail = true;
    server.use(http.get(REQUESTS, () => (fail ? HttpResponse.json({ error: "lookup failed" }, { status: 500 }) : undefined)));
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByRole("alert")).toHaveTextContent("The sign-in request could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByLabelText("Token name")).toBeInTheDocument();
  });
});

describe("Link page: typing the code", () => {
  it("asks for the code when the address has none", async () => {
    const calls = recordCalls();
    renderAt("/link");
    expect(await screen.findByLabelText("Enter the code from your terminal")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(calls).toEqual([]);
  });

  it.each(["brrw7q4k", "brrw-7q4k", " BRRW 7Q4K "])("accepts %j", async (typed) => {
    const calls = recordCalls();
    renderAt("/link");
    const field = await screen.findByLabelText("Enter the code from your terminal");
    await userEvent.click(field);
    await userEvent.paste(typed);
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByText(CODE)).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(`/link?code=${CODE}`);
    // The lookup moved the focus to the request, not to a button.
    expect(screen.getByRole("heading", { name: "Sign in a machine" })).toHaveFocus();
    expect(calls.map((c) => c.method)).toEqual(["GET"]);
  });

  it("looks the code up on Enter, and does nothing more", async () => {
    const calls = recordCalls();
    renderAt("/link");
    await userEvent.type(await screen.findByLabelText("Enter the code from your terminal"), "brrw7q4k{Enter}");
    expect(await screen.findByText(CODE)).toBeInTheDocument();
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.map((c) => c.method)).toEqual(["GET"]);
    expect(request().status).toBe("pending");
  });

  it("does not continue without a code", async () => {
    renderAt("/link");
    await screen.findByLabelText("Enter the code from your terminal");
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
  });
});

describe("Link page: signed out", () => {
  it("sends the visitor through the login and back, with the code", async () => {
    let signedIn = false;
    server.use(
      http.get("/api/v1/me", () => (signedIn ? HttpResponse.json(db.me) : HttpResponse.json({ error: "unauthorized" }, { status: 401 }))),
      http.post("/api/v1/auth/login", () => { signedIn = true; return HttpResponse.json({}); }),
    );
    const calls = recordCalls();
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByRole("heading", { name: "Sign in to Burrow" })).toBeInTheDocument();
    expect(calls).toEqual([]);

    await userEvent.type(screen.getByLabelText("Email"), "alice@acme.io");
    await userEvent.type(screen.getByLabelText("Password"), "password123");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByText(CODE)).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(`/link?code=${CODE}`);
    expect(screen.queryByRole("navigation")).toBeNull();
    // Signing in approved nothing.
    expect(calls.map((c) => c.method)).toEqual(["GET"]);
    expect(request().status).toBe("pending");
  });

  it("goes back to the login when the session ended while the page was open", async () => {
    renderAt(`/link?code=${CODE}`);
    await screen.findByText(CODE);
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json({ error: "unauthorized" }, { status: 401 })),
      http.post(`${REQUESTS}/approve`, () => HttpResponse.json({ error: "unauthorized" }, { status: 401 })),
    );
    await userEvent.click(approve());
    expect(await screen.findByRole("heading", { name: "Sign in to Burrow" })).toBeInTheDocument();
    expect(request().status).toBe("pending");
  });
});

describe("Link page: inside a frame", () => {
  it("shows nothing to press and asks nothing of the relay", async () => {
    vi.spyOn(window, "top", "get").mockReturnValue({} as Window);
    const calls = recordCalls();
    renderAt(`/link?code=${CODE}`);
    expect(await screen.findByRole("alert")).toHaveTextContent("Open this page in its own browser tab to continue.");
    expect(screen.queryByRole("button")).toBeNull();
    expect(calls).toEqual([]);
  });
});
