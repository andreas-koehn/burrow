import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { EditSlugDialog } from "@/components/EditSlugDialog";

const SVC = { id: "svc_web01", name: "web", slug: "k7p2qx" };

function mount(onOpenChange = vi.fn()) {
  const utils = renderApp(<EditSlugDialog service={SVC} open onOpenChange={onOpenChange} />);
  return { ...utils, onOpenChange };
}

async function submit(slug: string) {
  const field = screen.getByLabelText("URL slug");
  await userEvent.clear(field);
  await userEvent.type(field, slug);
  await userEvent.click(screen.getByRole("button", { name: "Change URL" }));
}

function failWith(status: number, message: string) {
  server.use(
    http.put("/api/v1/services/:id/slug", () => HttpResponse.json({ error: message }, { status })),
  );
}

describe("EditSlugDialog", () => {
  it("closes only after the service queries have been refetched", async () => {
    const { qc, onOpenChange } = mount();
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const invalidate = vi.spyOn(qc, "invalidateQueries").mockImplementation(() => gate);

    await submit("web");
    await waitFor(() => expect(invalidate).toHaveBeenCalledTimes(3));
    expect(invalidate.mock.calls.map(([f]) => f?.queryKey)).toEqual([
      ["services"], ["service", "svc_web01"], ["tunnels"],
    ]);
    // The refetch is still running: the dialog must stay open over the old URL.
    await new Promise((r) => setTimeout(r, 20));
    expect(onOpenChange).not.toHaveBeenCalled();

    release();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("shows a taken slug on the slug field", async () => {
    const { onOpenChange } = mount();
    await submit("ai4m2q"); // slug of svc_ai001 in the fixtures
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("slug already in use");
    expect(alert.id || alert.querySelector("[id]")?.id).toBe("edit-slug-err");
    expect(screen.getByLabelText("URL slug")).toHaveAttribute("aria-invalid", "true");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("shows a 400 on the slug field", async () => {
    failWith(400, "slug is reserved");
    mount();
    await submit("admin");
    expect(await screen.findByRole("alert")).toHaveTextContent("slug is reserved");
    expect(screen.getByLabelText("URL slug")).toHaveAttribute("aria-invalid", "true");
  });

  it.each([
    [403, "forbidden", "You don't have permission to change this service's URL."],
    [409, "slug requires an http service", "slug requires an http service"],
  ])("shows a %i in the dialog body, not on the slug field", async (status, message, shown) => {
    failWith(status, message);
    const { onOpenChange } = mount();
    await submit("web");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(shown);
    expect(alert).toHaveClass("notice-inline");
    expect(screen.getByLabelText("URL slug")).not.toHaveAttribute("aria-invalid");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("shows a network failure in the dialog body", async () => {
    server.use(http.put("/api/v1/services/:id/slug", () => HttpResponse.error()));
    mount();
    await submit("web");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't change the URL.");
    expect(alert).toHaveClass("notice-inline");
    expect(screen.getByLabelText("URL slug")).not.toHaveAttribute("aria-invalid");
  });

  it("editing the slug clears a previous error", async () => {
    failWith(403, "forbidden");
    mount();
    await submit("web");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("URL slug"), "2");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
