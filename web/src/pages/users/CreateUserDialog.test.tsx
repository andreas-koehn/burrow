import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import Users from "@/pages/Users";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { http, HttpResponse } from "msw";

describe("Create user dialog", () => {
  it("creates a user and shows it in the list", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await userEvent.click(screen.getByRole("button", { name: /^new user$/i }));
    const dialog = await screen.findByRole("dialog", { name: /^new user$/i });
    await userEvent.type(within(dialog).getByLabelText(/email/i), "dave@acme.io");
    await userEvent.type(within(dialog).getByLabelText(/password/i), "password123");
    await userEvent.click(within(dialog).getByRole("button", { name: /^create user$/i }));
    expect(await screen.findByText("dave@acme.io")).toBeInTheDocument();
  });

  it("Cancel uses the secondary button style (C3)", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await userEvent.click(screen.getByRole("button", { name: /^new user$/i }));
    const dialog = await screen.findByRole("dialog", { name: /^new user$/i });
    expect(within(dialog).getByRole("button", { name: "Cancel" }).className).toContain("btn-secondary");
  });

  it("shows the 409 duplicate-email error inline", async () => {
    renderApp(<Users />);
    await screen.findByText("bob@acme.io");
    await userEvent.click(screen.getByRole("button", { name: /^new user$/i }));
    const dialog = await screen.findByRole("dialog", { name: /^new user$/i });
    await userEvent.type(within(dialog).getByLabelText(/email/i), "bob@acme.io");
    await userEvent.type(within(dialog).getByLabelText(/password/i), "password123");
    await userEvent.click(within(dialog).getByRole("button", { name: /^create user$/i }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/already in use/i);
  });

  describe("roles", () => {
    it("offers custom roles with their description and posts the chosen one", async () => {
      db.roles.push({ name: "operator", description: "Runs the AI gateway", created_at: "2026-05-01T00:00:00Z", builtin: false });
      let posted: { role?: string } = {};
      server.use(http.post("/api/v1/users", async ({ request }) => {
        posted = (await request.json()) as { role?: string };
        return HttpResponse.json({ id: "u9", email: "e@acme.io", role: "operator", status: "active", last_login: null, created_at: "2026-05-01T00:00:00Z" }, { status: 201 });
      }));
      renderApp(<Users />);
      await screen.findByText("bob@acme.io");
      await userEvent.click(screen.getByRole("button", { name: /^new user$/i }));
      const dialog = await screen.findByRole("dialog", { name: /^new user$/i });
      const select = within(dialog).getByRole("combobox", { name: "Role" });
      await waitFor(async () => {
        await userEvent.click(select);
        expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Admin", "User", "analyst", "operator"]);
      });
      await userEvent.click(screen.getByRole("option", { name: "operator" }));
      expect(within(dialog).getByText("Runs the AI gateway")).toBeInTheDocument();
      await userEvent.type(within(dialog).getByLabelText(/email/i), "e@acme.io");
      await userEvent.type(within(dialog).getByLabelText(/password/i), "password123");
      await userEvent.click(within(dialog).getByRole("button", { name: /^create user$/i }));
      await waitFor(() => expect(posted.role).toBe("operator"));
    });

    it("asks for no roles without a roles permission and offers User and Admin", async () => {
      db.me = { ...db.me, role: "operator", permissions: ["users:manage", "users:read"] };
      let asked = 0;
      server.use(http.get("/api/v1/roles", () => { asked += 1; return HttpResponse.json(db.roles); }));
      renderApp(<Users />);
      await screen.findByText("bob@acme.io");
      await userEvent.click(screen.getByRole("button", { name: /^new user$/i }));
      const dialog = await screen.findByRole("dialog", { name: /^new user$/i });
      await userEvent.click(within(dialog).getByRole("combobox", { name: "Role" }));
      expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["User", "Admin"]);
      expect(asked).toBe(0);
    });
  });
});
