import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import { Routes, Route } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import { RequirePermission } from "./RequirePermission";

function mount(needs: string[]) {
  return renderApp(
    <Routes>
      <Route path="/page" element={<RequirePermission needs={needs}><p>AUDIT</p></RequirePermission>} />
    </Routes>,
    "/page",
  );
}

describe("RequirePermission", () => {
  it("shows the page to someone who holds the permission", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["audit:read"] };
    mount(["audit:read"]);
    expect(await screen.findByText("AUDIT")).toBeInTheDocument();
  });

  it("says which permission is missing", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["audit:read"] };
    mount(["webhooks:manage"]);
    expect(await screen.findByRole("heading", { name: "You cannot open this page" })).toBeInTheDocument();
    expect(screen.getByText("It needs the permission webhooks:manage. Ask an admin to add it to your role.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to your profile" })).toHaveAttribute("href", "/settings/profile");
    expect(screen.queryByText("AUDIT")).toBeNull();
  });

  it("names the alternatives when two permissions open the page", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["audit:read"] };
    mount(["users:read", "users:manage"]);
    expect(await screen.findByText("It needs one of the permissions users:read or users:manage. Ask an admin to add one to your role.")).toBeInTheDocument();
  });

  it("shows nothing while /me loads", () => {
    mount(["audit:read"]);
    expect(screen.queryByText("AUDIT")).toBeNull();
    expect(screen.queryByRole("heading", { name: "You cannot open this page" })).toBeNull();
  });
});
