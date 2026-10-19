import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import OAuthClients from "@/pages/settings/OAuthClients";

const mount = () => renderApp(<OAuthClients />, "/settings/oauth-clients");

describe("Settings → OAuth clients", () => {
  it("lists the registered clients with id and redirect addresses", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "OAuth clients" });
    expect(within(table).getByText("Cursor")).toBeInTheDocument();
    expect(within(table).getByText("bcl_0123456789abcdef0123456789abcdef")).toBeInTheDocument();
    expect(within(table).getByText("http://127.0.0.1/callback")).toBeInTheDocument();
  });

  it("creates a client from a name and redirect addresses and shows its id", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Add OAuth client" }));
    const dlg = await screen.findByRole("dialog");
    await userEvent.type(within(dlg).getByLabelText("Name"), "VS Code");
    await userEvent.type(within(dlg).getByLabelText("Redirect addresses"), "http://127.0.0.1/cb{Enter}https://vscode.example/cb");
    await userEvent.click(within(dlg).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(db.oauthClients.find((c) => c.name === "VS Code")).toBeTruthy());
    const c = db.oauthClients.find((x) => x.name === "VS Code")!;
    expect(c.redirect_uris).toEqual(["http://127.0.0.1/cb", "https://vscode.example/cb"]);
    expect(await within(await screen.findByRole("dialog")).findByText(c.client_id)).toBeInTheDocument();
  });

  it("shows the relay's reason for a bad address and keeps the form", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Add OAuth client" }));
    const dlg = await screen.findByRole("dialog");
    await userEvent.type(within(dlg).getByLabelText("Name"), "Bad");
    await userEvent.type(within(dlg).getByLabelText("Redirect addresses"), "ftp://x");
    await userEvent.click(within(dlg).getByRole("button", { name: "Create" }));
    expect(await within(dlg).findByRole("alert")).toHaveTextContent(/must be https/);
    expect(db.oauthClients).toHaveLength(1);
  });

  it("cannot create without a name or an address", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Add OAuth client" }));
    expect(within(await screen.findByRole("dialog")).getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("deletes after a confirmation that says its apps are cut off", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Delete Cursor" }));
    const dlg = await screen.findByRole("dialog");
    expect(dlg).toHaveTextContent(/connected app/i);
    expect(db.oauthClients).toHaveLength(1);
    await userEvent.click(within(dlg).getByRole("button", { name: "Delete client" }));
    await waitFor(() => expect(db.oauthClients).toHaveLength(0));
  });
});
