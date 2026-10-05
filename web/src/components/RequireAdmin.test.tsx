import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { RequireAdmin } from "./RequireAdmin";

function mount(me: () => Promise<Response>) {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url: unknown) => {
    if (String(url).includes("/api/v1/me")) return me();
    return new Response("{}", { status: 200 }) as Response;
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/settings/users"]}>
        <Routes>
          <Route path="/settings/users" element={<RequireAdmin><div>ADMIN PAGE</div></RequireAdmin>} />
          <Route path="/settings/profile" element={<div>PROFILE</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}
const as = (role: string) => async () =>
  new Response(JSON.stringify({ id: "u1", email: "alice@example.com", role }), { status: 200 }) as Response;

describe("RequireAdmin", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("shows the page to an admin", async () => {
    mount(as("admin"));
    expect(await screen.findByText("ADMIN PAGE")).toBeInTheDocument();
  });

  it("sends everyone else to their profile", async () => {
    mount(as("user"));
    expect(await screen.findByText("PROFILE")).toBeInTheDocument();
    expect(screen.queryByText("ADMIN PAGE")).toBeNull();
  });

  it("shows neither while the user is still loading", async () => {
    mount(() => new Promise<Response>(() => {}));
    await Promise.resolve();
    expect(screen.queryByText("ADMIN PAGE")).toBeNull();
    expect(screen.queryByText("PROFILE")).toBeNull();
  });
});
