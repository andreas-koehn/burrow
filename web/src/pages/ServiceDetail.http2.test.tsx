import { describe, it, expect, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { db, resetDb } from "@/mocks/db";
import ServiceDetail from "@/pages/ServiceDetail";

function mount(id: string) {
  return renderApp(<Routes><Route path="/services/:id" element={<ServiceDetail />} /></Routes>, `/services/${id}`);
}

beforeEach(() => resetDb());

describe("ServiceDetail of an http service: HTTP/2 and gRPC", () => {
  it("shows a Protocol card and a Host names card", async () => {
    mount("svc_ai001");
    const protocol = await screen.findByRole("region", { name: "Protocol" });
    expect(await within(protocol).findByRole("combobox", { name: "Upstream protocol" })).toBeInTheDocument();
    const hosts = await screen.findByRole("region", { name: "Host names" });
    expect(await within(hosts).findByText("foo.example.com")).toBeInTheDocument();
    expect(within(hosts).getByRole("button", { name: "New domain" })).toBeInTheDocument();
  });

  it("a tcp service has neither card", async () => {
    mount("svc_pg001");
    await screen.findByRole("heading", { name: "postgres" });
    expect(screen.queryByRole("region", { name: "Protocol" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Host names" })).toBeNull();
  });

  it("someone who may not configure the service sees no cards", async () => {
    db.me = { ...db.me, role: "user", permissions: ["tunnels:read:own"] };
    db.services.find((s) => s.id === "svc_ai001")!.user_id = db.me.id;
    mount("svc_ai001");
    await screen.findByRole("heading", { name: "ollama" });
    expect(screen.queryByRole("button", { name: "New domain" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });
});
