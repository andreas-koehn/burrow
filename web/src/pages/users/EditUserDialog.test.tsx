import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import Users from "@/pages/Users";
import { db } from "@/mocks/db";
import { server } from "@/mocks/server";
import { http, HttpResponse } from "msw";

describe("Edit user dialog", () => {
  it("suspends an active user via the status switch", async () => {
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await userEvent.click(within(dialog).getByRole("switch", { name: /user status/i }));
    await userEvent.click(within(dialog).getByRole("button", { name: /save changes/i }));
    const confirm = await screen.findByRole("dialog", { name: /suspend user/i });
    await userEvent.click(within(confirm).getByRole("button", { name: /^suspend user$/i }));
    await waitFor(() => {
      const row = screen.getByText("bob@acme.io").closest("tr")!;
      expect(within(row).getByText(/suspended/i)).toBeInTheDocument();
    });
  });

  it("asks before suspending, names the user, and sends nothing on Cancel", async () => {
    const patches: unknown[] = [];
    server.use(http.patch("/api/v1/users/:id", async ({ request }) => { patches.push(await request.json()); return HttpResponse.json({}); }));
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await userEvent.click(within(dialog).getByRole("switch", { name: /user status/i }));
    await userEvent.click(within(dialog).getByRole("button", { name: /save changes/i }));
    const confirm = await screen.findByRole("dialog", { name: /suspend user/i });
    expect(within(confirm).getByText(/bob@acme\.io/)).toBeInTheDocument();
    expect(patches).toEqual([]);
    await userEvent.click(within(confirm).getByRole("button", { name: /cancel/i }));
    expect(patches).toEqual([]);
  });

  it("saves a role change without asking", async () => {
    const patches: unknown[] = [];
    server.use(http.patch("/api/v1/users/:id", async ({ request }) => { patches.push(await request.json()); return HttpResponse.json({}); }));
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await userEvent.click(within(dialog).getByRole("button", { name: /save changes/i }));
    await waitFor(() => expect(patches).toHaveLength(1));
    expect(screen.queryByRole("dialog", { name: /suspend user/i })).toBeNull();
  });

  it("asks before deleting from the edit dialog, naming the user", async () => {
    const deletes: string[] = [];
    server.use(http.delete("/api/v1/users/:id", ({ params }) => { deletes.push(String(params.id)); return new HttpResponse(null, { status: 204 }); }));
    const confirmSpy = vi.spyOn(window, "confirm");
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await userEvent.click(within(dialog).getByRole("button", { name: /delete user/i }));
    const confirm = await screen.findByRole("dialog", { name: /delete user\?/i });
    expect(within(confirm).getByText(/bob@acme\.io/)).toBeInTheDocument();
    expect(deletes).toEqual([]);
    await userEvent.click(within(confirm).getByRole("button", { name: /^delete user$/i }));
    await waitFor(() => expect(deletes).toHaveLength(1));
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it("cancelling the delete confirmation sends nothing", async () => {
    const deletes: string[] = [];
    server.use(http.delete("/api/v1/users/:id", ({ params }) => { deletes.push(String(params.id)); return new HttpResponse(null, { status: 204 }); }));
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    await userEvent.click(within(dialog).getByRole("button", { name: /delete user/i }));
    const confirm = await screen.findByRole("dialog", { name: /delete user\?/i });
    await userEvent.click(within(confirm).getByRole("button", { name: /cancel/i }));
    expect(deletes).toEqual([]);
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

  it("keeps the current role under its plain name when the roles cannot be read", async () => {
    db.me = { ...db.me, permissions: (db.me.permissions ?? []).filter((p) => p !== "roles:read" && p !== "roles:manage") };
    db.users[1]!.role = "analyst";
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    const select = within(dialog).getByRole("combobox", { name: "Role" });
    expect(select).toHaveTextContent("analyst");
    expect(select).not.toHaveTextContent(/no longer exists/);
    await userEvent.click(select);
    expect(screen.getByRole("option", { name: "analyst" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /no longer exists/ })).toBeNull();
  });

  it("keeps the current role under its plain name when the roles fail to load", async () => {
    server.use(http.get("/api/v1/roles", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    db.users[1]!.role = "analyst";
    renderApp(<Users />);
    const bobRow = (await screen.findByText("bob@acme.io")).closest("tr")!;
    await userEvent.click(within(bobRow).getByRole("button", { name: /^edit$/i }));
    const dialog = await screen.findByRole("dialog", { name: /edit user/i });
    const select = within(dialog).getByRole("combobox", { name: "Role" });
    await waitFor(() => expect(select).toHaveTextContent("analyst"));
    expect(select).not.toHaveTextContent(/no longer exists/);
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
