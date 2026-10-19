import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import ConnectedApps from "@/pages/settings/ConnectedApps";
import { ALL_PERMISSIONS } from "@/auth/permissions";
import { grantAccess, resourceLabel } from "@/lib/oauth";

const mount = () => renderApp(<ConnectedApps />, "/settings/apps");

describe("oauth labels", () => {
  it("names a resource by its path", () => {
    expect(resourceLabel("https://r.example/mcp/burrow")).toBe("Burrow management");
    expect(resourceLabel("https://r.example/mcp/")).toBe("All tool sources");
    expect(resourceLabel("https://r.example/mcp/github")).toBe("Tool source github");
    expect(resourceLabel("https://r.example/other")).toBe("https://r.example/other");
  });
  it("says what a grant allows", () => {
    expect(grantAccess({ scopes: ["burrow:read"], allowed_tools: [], tools_read_only: false })).toBe("Read-only");
    expect(grantAccess({ scopes: ["burrow:write"], allowed_tools: [], tools_read_only: false })).toBe("Full access");
    expect(grantAccess({ scopes: ["mcp:tools"], allowed_tools: ["a__*"], tools_read_only: true })).toBe("a__*, read-only tools only");
    expect(grantAccess({ scopes: ["mcp:tools"], allowed_tools: [], tools_read_only: false })).toBe("no tools");
  });
});

describe("Settings → Connected apps", () => {
  it("lists the caller's own apps with what each may reach", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Connected apps" });
    expect(within(table).getByText("Claude Desktop")).toBeInTheDocument();
    expect(within(table).getByText("Burrow management")).toBeInTheDocument();
    expect(within(table).getByText("Read-only")).toBeInTheDocument();
    expect(within(table).getByText("All tool sources")).toBeInTheDocument();
    expect(within(table).queryByText("bob@acme.io")).toBeNull();
    expect(screen.getAllByRole("button", { name: /^Revoke/ })).toHaveLength(2);
  });

  it("revokes only after a confirmation", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Revoke Claude Desktop" }));
    const dlg = await screen.findByRole("dialog");
    expect(db.oauthGrants.some((g) => g.id === "og_1")).toBe(true);
    await userEvent.click(within(dlg).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(db.oauthGrants.some((g) => g.id === "og_1")).toBe(false));
    await waitFor(() => expect(screen.queryByText("Claude Desktop")).toBeNull());
  });

  it("cancel keeps the app", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Revoke Claude Desktop" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(db.oauthGrants.some((g) => g.id === "og_1")).toBe(true);
  });

  it("offers an admin everyone's apps", async () => {
    mount();
    const all = await screen.findByRole("switch", { name: "All users" });
    await userEvent.click(all);
    const table = await screen.findByRole("table", { name: "Connected apps" });
    expect(await within(table).findByText("bob@acme.io")).toBeInTheDocument();
    expect(within(table).getAllByRole("columnheader").map((c) => c.textContent)).toContain("User");
  });

  it("has no everyone toggle without the permission", async () => {
    db.me.role = "user";
    db.me.permissions = ALL_PERMISSIONS.filter((p) => p !== "oauth:clients:manage");
    mount();
    await screen.findByRole("table", { name: "Connected apps" });
    expect(screen.queryByRole("switch", { name: "All users" })).toBeNull();
  });

  it("explains an empty list", async () => {
    db.oauthGrants = [];
    mount();
    expect(await screen.findByText("No connected apps")).toBeInTheDocument();
  });
});
