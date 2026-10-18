import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { renderApp } from "@/mocks/test-utils";
import ServiceDetail from "@/pages/ServiceDetail";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { USER_PERMISSIONS } from "@/auth/permissions";


function mountAt(id: string) {
  return renderApp(
    <Routes>
      <Route path="/services" element={<h1>Services list</h1>} />
      <Route path="/services/:id" element={<ServiceDetail />} />
    </Routes>,
    `/services/${id}`,
  );
}

async function openDialog() {
  await userEvent.click(await screen.findByRole("button", { name: "Delete service" }));
  return screen.findByRole("dialog");
}

describe("ServiceDetail: delete service", () => {
  it("offers the action in a danger area at the bottom", async () => {
    mountAt("svc_graf01");
    const zone = await screen.findByRole("region", { name: "Danger zone" });
    expect(within(zone).getByRole("button", { name: "Delete service" })).toHaveAttribute("type", "button");
  });

  it("is not offered without the permission to configure the service", async () => {
    db.me = { ...db.me, role: "user", permissions: USER_PERMISSIONS.filter((x) => !x.startsWith("services:configure")) };
    mountAt("svc_graf01");
    await screen.findByRole("heading", { level: 1, name: "grafana" });
    expect(screen.queryByRole("button", { name: "Delete service" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Danger zone" })).toBeNull();
  });

  it("asks first, naming the service and what happens", async () => {
    mountAt("svc_graf01");
    const dialog = await openDialog();
    expect(dialog).toHaveTextContent("grafana");
    expect(dialog).toHaveTextContent(/URL stops working/i);
    expect(dialog).toHaveTextContent(/connected clients are disconnected/i);
    expect(db.services.some((s) => s.id === "svc_graf01")).toBe(true);
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(db.services.some((s) => s.id === "svc_graf01")).toBe(true);
  });

  it("deletes, goes to the list and says so", async () => {
    const success = vi.spyOn(toast, "success").mockImplementation(() => 1);
    mountAt("svc_graf01");
    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete service" }));
    expect(await screen.findByRole("heading", { name: "Services list" })).toBeInTheDocument();
    expect(db.services.some((s) => s.id === "svc_graf01")).toBe(false);
    expect(success).toHaveBeenCalledWith(expect.stringContaining("grafana"));
  });

  it("shows the 409 in the dialog and stays", async () => {
    mountAt("svc_ai001");
    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete service" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/delete the provider/i);
    expect(screen.queryByRole("heading", { name: "Services list" })).toBeNull();
    expect(db.services.some((s) => s.id === "svc_ai001")).toBe(true);
  });

  it("shows any other failure in the dialog", async () => {
    server.use(http.delete("/api/v1/services/:id", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mountAt("svc_graf01");
    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete service" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("boom");
  });
});
