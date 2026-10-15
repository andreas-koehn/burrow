import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import { useAuth } from "./useAuth";
import { useCan } from "./useCan";

function Probe() {
  const { loading } = useAuth();
  const audit = useCan("audit:read");
  const access = useCan(["users:manage", "roles:manage"]);
  if (loading) return <p>loading</p>;
  return <p>{String(audit)} / {String(access)}</p>;
}

describe("useCan", () => {
  it("answers from the permissions on /me", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["audit:read"] };
    renderApp(<Probe />);
    expect(await screen.findByText("true / false")).toBeInTheDocument();
  });

  it("falls back to the role on a relay whose /me has no permissions", async () => {
    const { permissions: _drop, ...bare } = db.me;
    db.me = { ...bare, role: "admin" };
    renderApp(<Probe />);
    expect(await screen.findByText("true / true")).toBeInTheDocument();
  });
});
