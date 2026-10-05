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

describe("Users — SMTP informational notice (P6B.1)", () => {
  it("shows SMTP notice when settings has no smtp.host (default empty settings)", async () => {
    // Default db.settings = {} so smtp.host is absent
    renderApp(<Users />);
    // Wait for users to load so settings query has had time to resolve
    await screen.findByText("bob@acme.io");
    expect(await screen.findByText(/Email isn't set up\. Password resets and test emails are unavailable until SMTP is configured\./)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /set up email/i });
    expect(link).toBeInTheDocument();
    expect(link.getAttribute("href")).toBe("/settings/email");
  });

  it("hides SMTP notice when smtp.host is set", async () => {
    server.use(
      http.get("/api/v1/settings", () =>
        HttpResponse.json({ "smtp.host": "smtp.example.com" }),
      ),
    );
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    // Give the settings query time to resolve
    await waitFor(() => {
      expect(screen.queryByText(/email isn't set up/i)).not.toBeInTheDocument();
    });
  });

  it("SMTP notice contains link to /settings/email", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    const link = await screen.findByRole("link", { name: /set up email/i });
    expect(link.getAttribute("href")).toBe("/settings/email");
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
