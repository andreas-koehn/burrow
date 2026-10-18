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
    expect(dialog).toHaveTextContent(/registers the same tunnel name later creates a new service/i);
    expect(dialog).toHaveTextContent(/settings made here do not come back/i);
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
    server.use(http.delete("/api/v1/services/:id", () =>
      HttpResponse.json({ error: "the service is connected; stop the client or remove the tunnel from its config, then delete" }, { status: 409 })));
    mountAt("svc_graf01");
    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete service" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/the service is connected/i);
    expect(screen.queryByRole("heading", { name: "Services list" })).toBeNull();
    expect(db.services.some((s) => s.id === "svc_graf01")).toBe(true);
  });

  it("is disabled, with a hint, while a client serves the service", async () => {
    // svc_ai001 is online; take its provider away so only the client is in the way.
    db.aiProviders = db.aiProviders.filter((p) => p.service_id !== "svc_ai001");
    mountAt("svc_ai001");
    const zone = await screen.findByRole("region", { name: "Danger zone" });
    const button = within(zone).getByRole("button", { name: "Delete service" });
    expect(button).toBeDisabled();
    expect(button).toHaveAccessibleDescription(/stop the client or remove the tunnel from its config/i);
  });

  it("is disabled, with a hint, while a provider uses the service", async () => {
    const svc = db.services.find((s) => s.id === "svc_ai001")!;
    svc.connected = false;
    mountAt("svc_ai001");
    const zone = await screen.findByRole("region", { name: "Danger zone" });
    await waitFor(() => expect(within(zone).getByRole("button", { name: "Delete service" })).toBeDisabled());
    expect(within(zone).getByRole("button", { name: "Delete service" })).toHaveAccessibleDescription(/delete the provider/i);
  });

  it("leaves the page before the service is asked for again", async () => {
    let reads = 0;
    let deleted = false;
    server.use(
      http.get("/api/v1/services/:id", ({ params }) => {
        if (deleted) reads++;
        const svc = db.services.find((s) => s.id === params.id);
        return svc ? HttpResponse.json({ ...svc, api_key_count: 0, access_policy: [], status: "offline", provider_slug: "" }) : HttpResponse.json({ error: "service not found" }, { status: 404 });
      }),
      http.delete("/api/v1/services/:id", ({ params }) => {
        deleted = true;
        db.services = db.services.filter((s) => s.id !== params.id);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    mountAt("svc_graf01");
    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete service" }));
    expect(await screen.findByRole("heading", { name: "Services list" })).toBeInTheDocument();
    await new Promise((r) => setTimeout(r, 300));
    expect(reads).toBe(0);
  });

  it("shows any other failure in the dialog", async () => {
    server.use(http.delete("/api/v1/services/:id", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mountAt("svc_graf01");
    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete service" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("boom");
  });
});
