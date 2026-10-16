import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import Users from "@/pages/Users";
import { db } from "@/mocks/db";

describe("Edit user dialog", () => {
  it("suspends an active user via the status switch", async () => {
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await userEvent.click(within(dialog).getByRole("switch", { name: /user status/i }));
    await userEvent.click(within(dialog).getByRole("button", { name: /save changes/i }));
    await waitFor(() => {
      const row = screen.getByText("bob@acme.io").closest("tr")!;
      expect(within(row).getByText(/suspended/i)).toBeInTheDocument();
    });
  });

  it("disables status switch and delete for the current user (self)", async () => {
    renderApp(<Users />);
    const meRow = (await screen.findByText("alice@acme.io")).closest("tr")!;
    await userEvent.click(within(meRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    expect(within(dialog).getByRole("switch", { name: /user status/i })).toHaveAttribute("aria-disabled", "true");
    expect(within(dialog).getByRole("button", { name: /delete user/i })).toBeDisabled();
  });

  it("keeps a role that no longer exists selectable", async () => {
    db.users[1]!.role = "ghost";
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    const select = within(dialog).getByRole("combobox", { name: "Role" });
    await waitFor(() => expect(select).toHaveTextContent("ghost (no longer exists)"));
    await userEvent.click(select);
    expect(screen.getByRole("option", { name: "ghost (no longer exists)" })).toBeInTheDocument();
  });

  it("offers custom roles", async () => {
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await waitFor(async () => {
      await userEvent.click(within(dialog).getByRole("combobox", { name: "Role" }));
      expect(screen.getByRole("option", { name: "analyst" })).toBeInTheDocument();
    });
  });
});
