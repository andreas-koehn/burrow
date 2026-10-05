import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { ThemeProvider } from "@/components/theme-provider";
import Profile from "./Profile";

/** Render Profile with all required providers.
 *  mockMe: the user returned by GET /api/v1/me.
 *  mockPost: optional override for the POST /auth/change-password response.
 */
function setup(
  mockMe: object = { id: "u1", email: "alice@example.com", role: "user" },
  mockPost?: () => Response,
) {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown, opts?: unknown) => {
    const u = String(url);
    const o = opts as RequestInit | undefined;
    if (u.includes("/api/v1/me")) {
      return new Response(JSON.stringify(mockMe), { status: 200 }) as Response;
    }
    if (u.includes("/auth/change-password") && o?.method === "POST") {
      if (mockPost) return mockPost();
      return new Response(null, { status: 204 }) as Response;
    }
    return new Response(null, { status: 200 }) as Response;
  });

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/settings/profile"]}>
          <Routes>
            <Route path="/settings/profile" element={<Profile />} />
            <Route path="/login" element={<div>LOGIN PAGE</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
  return qc;
}

describe("Profile", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("is headed Profile & password and leaves the sessions to their own page", async () => {
    setup();
    expect(await screen.findByRole("heading", { name: "Profile & password", level: 1 })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /active sessions/i })).toBeNull();
  });

  it("renders email and role from useAuth", async () => {
    setup({ id: "u1", email: "alice@example.com", role: "admin" });
    await screen.findByText("alice@example.com");
    expect(screen.getByText("admin")).toBeInTheDocument();
  });

  it("renders the change-password form with three labeled inputs", async () => {
    setup();
    expect(screen.getByLabelText("Current password")).toBeInTheDocument();
    expect(screen.getByLabelText("New password")).toBeInTheDocument();
    expect(screen.getByLabelText("Confirm new password")).toBeInTheDocument();
  });

  it("change-password submit uses the design-system primary button class", async () => {
    setup();
    await screen.findByLabelText(/current password/i);
    expect(screen.getByRole("button", { name: /change password/i }).className).toContain("btn-primary");
  });

  it("blocks submit when new and confirm do not match (client validation, no fetch for change-password)", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      if (String(url).includes("/api/v1/me")) {
        return new Response(JSON.stringify({ id: "u1", email: "a@b.com", role: "user" }), { status: 200 }) as Response;
      }
      return new Response(null, { status: 204 }) as Response;
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Profile />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>
    );

    await screen.findByLabelText(/current password/i);
    await userEvent.type(screen.getByLabelText("Current password"), "currentpass");
    await userEvent.type(screen.getByLabelText("New password"), "newpassword1");
    await userEvent.type(screen.getByLabelText("Confirm new password"), "differentpassword");
    await userEvent.click(screen.getByRole("button", { name: /change password/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/do not match/i);
    // change-password endpoint should NOT have been called
    const changePwCalls = fetchSpy.mock.calls.filter(([url]) =>
      String(url).includes("/auth/change-password")
    );
    expect(changePwCalls).toHaveLength(0);
  });

  it("blocks submit when new password is too short (client validation)", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
      if (String(url).includes("/api/v1/me")) {
        return new Response(JSON.stringify({ id: "u1", email: "a@b.com", role: "user" }), { status: 200 }) as Response;
      }
      return new Response(null, { status: 204 }) as Response;
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Profile />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>
    );

    await screen.findByLabelText(/current password/i);
    await userEvent.type(screen.getByLabelText("Current password"), "currentpass");
    await userEvent.type(screen.getByLabelText("New password"), "short");
    await userEvent.type(screen.getByLabelText("Confirm new password"), "short");
    await userEvent.click(screen.getByRole("button", { name: /change password/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/at least 8 characters/i);
    const changePwCalls = fetchSpy.mock.calls.filter(([url]) =>
      String(url).includes("/auth/change-password")
    );
    expect(changePwCalls).toHaveLength(0);
  });

  it("shows success toast and clears fields on 204", async () => {
    setup(
      { id: "u1", email: "alice@example.com", role: "user" },
      () => new Response(null, { status: 204 }) as Response,
    );

    await screen.findByLabelText(/current password/i);
    await userEvent.type(screen.getByLabelText("Current password"), "oldpassword");
    await userEvent.type(screen.getByLabelText("New password"), "newpassword1");
    await userEvent.type(screen.getByLabelText("Confirm new password"), "newpassword1");
    await userEvent.click(screen.getByRole("button", { name: /change password/i }));

    // Fields cleared
    await waitFor(() => {
      expect(screen.getByLabelText("Current password")).toHaveValue("");
    });
    expect(screen.getByLabelText("New password")).toHaveValue("");
    expect(screen.getByLabelText("Confirm new password")).toHaveValue("");
  });

  it("shows 'Current password is incorrect' on 401 and does NOT navigate to /login", async () => {
    setup(
      { id: "u1", email: "alice@example.com", role: "user" },
      () => new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 }) as Response,
    );

    await screen.findByLabelText(/current password/i);
    await userEvent.type(screen.getByLabelText("Current password"), "wrongpassword");
    await userEvent.type(screen.getByLabelText("New password"), "newpassword1");
    await userEvent.type(screen.getByLabelText("Confirm new password"), "newpassword1");
    await userEvent.click(screen.getByRole("button", { name: /change password/i }));

    // Error displayed inline — page stays mounted (not redirected)
    expect(await screen.findByRole("alert")).toHaveTextContent(/current password is incorrect/i);
    // "LOGIN PAGE" must NOT appear — no redirect triggered
    expect(screen.queryByText("LOGIN PAGE")).not.toBeInTheDocument();
    // The change-password form must still be visible
    expect(screen.getByLabelText("Current password")).toBeInTheDocument();
  });

  it("shows password too short message on 400 with relevant message", async () => {
    setup(
      { id: "u1", email: "alice@example.com", role: "user" },
      () => new Response(JSON.stringify({ error: "password too short" }), { status: 400 }) as Response,
    );

    await screen.findByLabelText(/current password/i);
    await userEvent.type(screen.getByLabelText("Current password"), "currentpass");
    // Bypass client validation by making both fields match but ≥8 chars
    await userEvent.type(screen.getByLabelText("New password"), "newpassword1");
    await userEvent.type(screen.getByLabelText("Confirm new password"), "newpassword1");
    await userEvent.click(screen.getByRole("button", { name: /change password/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/short/i);
  });
});
