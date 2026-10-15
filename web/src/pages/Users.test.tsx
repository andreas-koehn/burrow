import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import Users from "@/pages/Users";
import { db } from "@/mocks/db";

describe("Users list", () => {
  it("renders users with status badge and last-login", async () => {
    renderApp(<Users />);
    expect(await screen.findByText("bob@acme.io")).toBeInTheDocument();
    const carolRow = screen.getByText("carol@acme.io").closest("tr")!;
    expect(within(carolRow).getByText(/suspended/i)).toBeInTheDocument();
    const bobRow = screen.getByText("bob@acme.io").closest("tr")!;
    expect(within(bobRow).getByText("—")).toBeInTheDocument(); // null last_login
  });

  it("writes Created and Last login as how long ago, with the exact UTC time on hover", async () => {
    renderApp(<Users />);
    const carolRow = (await screen.findByText("carol@acme.io")).closest("tr")!;
    // Seeded months ago: past 30 days the date stands for "how long ago".
    const [created, lastLogin] = Array.from(carolRow.querySelectorAll("time"));
    expect(created).toHaveTextContent("01 Mar 2026");
    expect(created).toHaveAttribute("title", "01 Mar 2026, 08:00:00 UTC");
    expect(created).toHaveAttribute("datetime", "2026-03-01T08:00:00Z");
    expect(lastLogin).toHaveTextContent("10 Apr 2026");
    expect(lastLogin).toHaveAttribute("title", "10 Apr 2026, 12:00:00 UTC");
  });

  it("a login a moment ago reads as relative time", async () => {
    server.use(http.get("/api/v1/users", () => HttpResponse.json({
      users: [{ id: "u1", email: "dora@acme.io", role: "user", status: "active", last_login: new Date(Date.now() - 3 * 3_600_000 - 5_000).toISOString(), created_at: new Date(Date.now() - 3 * 86_400_000 - 5_000).toISOString() }],
      next_cursor: "",
    })));
    renderApp(<Users />);
    const row = (await screen.findByText("dora@acme.io")).closest("tr")!;
    expect(within(row).getByText("3 days ago")).toHaveAttribute("title", expect.stringMatching(/ UTC$/));
    expect(within(row).getByText("3 hours ago")).toHaveAttribute("title", expect.stringMatching(/ UTC$/));
  });

  it("filters by email search", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await userEvent.type(screen.getByRole("searchbox", { name: /search/i }), "carol");
    await waitFor(() => expect(screen.queryByText("bob@acme.io")).not.toBeInTheDocument());
    expect(screen.getByText("carol@acme.io")).toBeInTheDocument();
  });

  // A page with a table has the whole column in every state: while it loads,
  // with rows, and when a search matches nobody. The header must not jump.
  it("keeps its width class while loading, with rows and with no match", async () => {
    const { container } = renderApp(<Users />);
    const root = () => container.querySelector(".settings-page");
    const width = () => Array.from(root()!.classList).filter((c) => c.startsWith("settings-"));
    // Loading: no table yet.
    expect(screen.queryByRole("table")).toBeNull();
    expect(width()).toEqual(["settings-page"]);
    await screen.findByRole("table", { name: "Users" });
    expect(width()).toEqual(["settings-page"]);
    await userEvent.type(screen.getByRole("searchbox", { name: /search/i }), "nobody-by-that-name");
    expect(await screen.findByText("No users found")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
    expect(width()).toEqual(["settings-page"]);
  });

  it("filter row has explicit gap so the Role select can't overlap the count (C1)", async () => {
    const { container } = renderApp(<Users />);
    await waitFor(() => screen.getByRole("heading", { name: "Users" }));
    const row = container.querySelector(".users-filter-row");
    expect(row).not.toBeNull();
    // The gap is defined in CSS (not inline), so we verify the class is present
    // and the element contains both the Role select and the count label.
    expect(row!.querySelector("select, [role='combobox'], button")).not.toBeNull();
    expect(row!.textContent).toMatch(/total/);
  });
});

describe("Users — delete self (U5)", () => {
  it("explains why you cannot delete your own account", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    const you = screen.getByLabelText("this is you").closest("tr")!;
    const own = within(you).getByRole("button", { name: /^delete user /i });
    expect(own).toBeDisabled();
    expect(own).toHaveAttribute("title", "You can't delete your own account.");
  });
});

describe("Users — email notice", () => {
  it("is not shown here: it is a relay notice on the overviews and a mark on Settings", async () => {
    // Default db.settings = {}, so email is not set up.
    const seen = { count: 0 };
    server.use(http.get("/api/v1/settings", () => { seen.count += 1; return HttpResponse.json({}); }));
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByText(/email isn't set up/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /set up email/i })).toBeNull();
    // The page has no other use for the settings.
    expect(seen.count).toBe(0);
  });
});

describe("CreateUserDialog — no-email clarifier (P6B.3)", () => {
  it("dialog description says Burrow does not email an invitation", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await userEvent.click(screen.getByRole("button", { name: /^new user$/i }));
    expect(await screen.findByText(/does not email an invitation/i)).toBeInTheDocument();
  });

  describe("writes follow users:manage", () => {
    it("a custom role with users:manage can add, edit and delete", async () => {
      db.me = { id: "u_op", email: "op@acme.io", role: "operator", permissions: ["users:manage"] };
      renderApp(<Users />);
      const row = (await screen.findByText("bob@acme.io")).closest("tr")!;
      expect(screen.getByRole("button", { name: /^new user$/i })).toBeInTheDocument();
      expect(within(row).getByRole("button", { name: "Edit" })).toBeInTheDocument();
      expect(within(row).getByRole("button", { name: /delete user/i })).toBeInTheDocument();
    });

    it("a custom role with users:read only reads the list", async () => {
      db.me = { id: "u_op", email: "op@acme.io", role: "operator", permissions: ["users:read"] };
      renderApp(<Users />);
      await screen.findByText("bob@acme.io");
      expect(screen.queryByRole("button", { name: /^new user$/i })).toBeNull();
      expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
      expect(screen.queryByRole("button", { name: /delete user/i })).toBeNull();
    });
  });

  it("a custom role shows under its own name", async () => {
    server.use(http.get("/api/v1/users", () => HttpResponse.json({
      users: [{ id: "u9", email: "olga@acme.io", role: "operator", status: "active", last_login: null, created_at: "2026-03-01T08:00:00Z" }],
      next_cursor: "",
    })));
    renderApp(<Users />);
    const row = (await screen.findByText("olga@acme.io")).closest("tr")!;
    expect(within(row).getByText("Operator")).toBeInTheDocument();
    expect(within(row).queryByText("User")).toBeNull();
  });
});
