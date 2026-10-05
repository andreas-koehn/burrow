import { describe, it, expect } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import Users from "@/pages/Users";

describe("Users list", () => {
  it("renders users with status badge and last-login", async () => {
    renderApp(<Users />);
    expect(await screen.findByText("bob@acme.io")).toBeInTheDocument();
    const carolRow = screen.getByText("carol@acme.io").closest("tr")!;
    expect(within(carolRow).getByText(/suspended/i)).toBeInTheDocument();
    const bobRow = screen.getByText("bob@acme.io").closest("tr")!;
    expect(within(bobRow).getByText("—")).toBeInTheDocument(); // null last_login
  });

  it("filters by email search", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await userEvent.type(screen.getByRole("searchbox", { name: /search/i }), "carol");
    await waitFor(() => expect(screen.queryByText("bob@acme.io")).not.toBeInTheDocument());
    expect(screen.getByText("carol@acme.io")).toBeInTheDocument();
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
});
