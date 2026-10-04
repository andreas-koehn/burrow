import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import Users from "@/pages/Users";

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
});
