import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import Services from "@/pages/Services";

function mount() {
  return renderApp(<Services />, "/services");
}

describe("Services page", () => {
  it("shows a loading skeleton before data arrives", () => {
    const { container } = mount();
    expect(container.querySelector(".skel")).toBeTruthy();
  });

  it("renders a row per service with name, type, hostname, access badge", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    expect(within(web).getByText("http")).toBeInTheDocument();
    // http row: hostname shown mono with an aria-labelled copy button
    expect(within(web).getByText("k7p2qx.tunnels.example.com")).toBeInTheDocument();
    expect(
      within(web).getByRole("button", { name: /copy hostname k7p2qx\.tunnels\.example\.com/i }),
    ).toBeInTheDocument();
    expect(within(web).getByText("Open")).toBeInTheDocument();

    const ai = within(table).getByText("ollama").closest("tr")!;
    expect(within(ai).getByText("API key")).toBeInTheDocument();

    const gf = within(table).getByText("grafana").closest("tr")!;
    expect(within(gf).getByText("Burrow login")).toBeInTheDocument();

    // tcp row: no hostname, em-dash, no copy button
    const pg = within(table).getByText("postgres").closest("tr")!;
    expect(within(pg).getByText("tcp")).toBeInTheDocument();
    expect(within(pg).queryByRole("button", { name: /copy hostname/i })).toBeNull();
  });

  it("Configure opens the AccessModePanel for that service", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /services/i });
    const web = within(table).getByText("web").closest("tr")!;
    await userEvent.click(within(web).getByRole("button", { name: /configure/i }));
    expect(await screen.findByRole("radiogroup", { name: /access mode/i })).toBeInTheDocument();
  });

  it("shows the empty state when there are no services", async () => {
    db.services = [];
    mount();
    // EmptyState renders <h4>title</h4>; body text is in a <p> with <code> children.
    expect(await screen.findByText("No services yet")).toBeInTheDocument();
    // Verify at least one keyword from the body is present in the document.
    expect(screen.getByText("burrow connect")).toBeInTheDocument();
  });

  it("renders idle services as a status-idle badge, not bare muted text (D-12/L-15)", async () => {
    mount();
    const table = await screen.findByRole("table");
    const gf = within(table).getByText("grafana").closest("tr")!;
    const idle = within(gf).getByText("idle");
    expect(idle).toHaveClass("badge", "status-idle");
    expect(idle).not.toHaveClass("muted");
  });

  it("shows an error notice with Retry on failure", async () => {
    server.use(
      http.get("/api/v1/services", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
    );
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/boom|couldn't load/i);
    const retry = screen.getByRole("button", { name: /retry/i });
    // Restore a working handler, click Retry, expect the table to appear.
    server.resetHandlers();
    await userEvent.click(retry);
    await waitFor(() => expect(screen.getByRole("table", { name: /services/i })).toBeInTheDocument());
  });

  // P3A.2 — Services explainer
  it("shows a durable-config explainer with a link to /tunnels (role=note, NOT alert)", async () => {
    mount();
    // explainer is always rendered — wait for it after mount
    const explainer = await screen.findByText(/durable saved config/i);
    expect(explainer).toBeInTheDocument();
    // must contain a link to /tunnels
    const wrapper = explainer.closest("[role='note']") ?? explainer.parentElement!;
    const link = wrapper.querySelector("a[href='/tunnels']");
    expect(link).not.toBeNull();
    // must NOT be a role=alert (that role is reserved for the error notice)
    expect(screen.queryByRole("alert", { name: /durable saved config/i })).toBeNull();
  });

  it("creates a new service via the + New service dialog and shows a success toast", async () => {
    mount();
    // Wait for initial data to load (table must be present first)
    await screen.findByRole("table", { name: /services/i });

    // Open the create dialog
    await userEvent.click(screen.getByRole("button", { name: /\+ new service/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    // Fill in the service ID and an optional title
    const idInput = screen.getByLabelText(/service id/i);
    await userEvent.type(idInput, "ai-svc");
    const titleInput = screen.getByLabelText(/title/i);
    await userEvent.type(titleInput, "My AI Service");

    // Click Create
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));

    // Success toast appears with the service_id
    await screen.findByText("Service ai-svc created.");

    // Dialog should be closed
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    // The new service appears in the table on refetch (by its title/name)
    const table = await screen.findByRole("table", { name: /services/i });
    expect(within(table).getByText("My AI Service")).toBeInTheDocument();
  });
});
