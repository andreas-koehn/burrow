import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import GatewayKeys from "@/pages/GatewayKeys";

function mount() {
  return renderApp(<GatewayKeys />, "/gateway/keys");
}

function mountAs(role: "admin" | "user") {
  db.me = { ...db.me, role };
  return mount();
}

const otherUsersKey = () => db.aiGatewayKeys.push({
  id: "gk_bob", name: "bobs-ci", key_prefix: "bgw_Zz9y", user_id: "bur_usr_bob0002", allowed_models: ["burrow-simple", "ollama/*"],
  last_used: null, created_at: "2026-05-11T08:00:00Z", revoked_at: null,
});

describe("Gateway keys page", () => {
  it("creates a key, shows it once, and never again", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const { qc } = mount();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await user.type(within(dialog).getByLabelText("Name"), "laptop-2");
    await user.click(within(dialog).getByRole("button", { name: "Create key" }));
    const secret = await screen.findByLabelText("Your new key");
    expect((secret as HTMLInputElement).value).toMatch(/^bgw_/);
    expect(secret).toHaveAttribute("readonly");
    expect(screen.getByText("Copy it now. It is not shown again.")).toBeInTheDocument();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    await user.click(screen.getByRole("button", { name: "Copy key" }));
    expect(writeText).toHaveBeenCalledWith("bgw_" + "x".repeat(43));
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(document.body.innerHTML).not.toMatch(/bgw_x{10,}/);
    expect(await screen.findByText("laptop-2")).toBeInTheDocument();
    // Nothing kept it: not the caches, not the browser's storage, not the address.
    const cached = JSON.stringify([
      qc.getQueryCache().getAll().map((q) => q.state.data),
      qc.getMutationCache().getAll().map((m) => [m.state.data, m.state.variables]),
    ]);
    expect(cached).not.toMatch(/bgw_x{10,}/);
    expect(setItem.mock.calls.flat().join(" ")).not.toMatch(/bgw_x{10,}/);
    expect(window.location.href).not.toMatch(/bgw_/);
    setItem.mockRestore();
  });

  it("drops the key when the dialog is closed any other way", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop-3");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await screen.findByLabelText("Your new key");
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.body.innerHTML).not.toMatch(/bgw_x{10,}/);
    // Opening it again starts over.
    await userEvent.click(screen.getByRole("button", { name: "New key" }));
    expect(within(await screen.findByRole("dialog", { name: "New gateway key" })).getByLabelText("Name")).toHaveValue("");
    expect(screen.queryByLabelText("Your new key")).toBeNull();
  });

  it("restricts a key to chosen models", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/keys", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "gk9", name: "ci", key: "bgw_" + "y".repeat(43), allowed_models: ["burrow-simple", "ollama/*"] }, { status: 201 });
    }));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "ci");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Only these models" }));
    // An empty list would mean every model: nothing chosen is not a restriction.
    expect(within(dialog).getByRole("button", { name: "Create key" })).toBeDisabled();
    // What each kind of entry opens.
    expect(await within(dialog).findByText(/A model name allows that model/)).toHaveTextContent(
      "A model name allows that model on the gateway's endpoints (/openai/v1, /anthropic), not on a provider's own address /ai/<provider>/…. \"Everything from\" a provider allows both",
    );
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "burrow-simple" }));
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Everything from ollama" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await waitFor(() => expect(posted).toMatchObject({ name: "ci", allowed_models: ["burrow-simple", "ollama/*"] }));
  });

  it("an unrestricted key sends no list", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/keys", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "gk9", name: "all", key: "bgw_" + "y".repeat(43), allowed_models: [] }, { status: 201 });
    }));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    expect(within(dialog).getByRole("radio", { name: "All models" })).toBeChecked();
    await userEvent.type(within(dialog).getByLabelText("Name"), "all");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await waitFor(() => expect(posted).toEqual({ name: "all" }));
  });

  it("shows the server's reason when a key cannot be created", async () => {
    server.use(http.post("/api/v1/ai/keys", () => HttpResponse.json({ error: "gateway keys are created from a dashboard session" }, { status: 403 })));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("gateway keys are created from a dashboard session");
    expect(screen.queryByLabelText("Your new key")).toBeNull();
  });

  it("lists a key's prefix, what it may use, and whether it is active", async () => {
    otherUsersKey();
    mount();
    const table = await screen.findByRole("table", { name: "Gateway keys" });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(
      expect.arrayContaining(["Name", "Key", "Models", "Last used", "Status", "Owner"]),
    );
    const mine = within(table).getByRole("row", { name: /laptop/ });
    expect(within(mine).getByText("bgw_Ab3d…")).toBeInTheDocument();
    expect(within(mine).getByText("all")).toBeInTheDocument();
    expect(within(mine).getByText("active")).toBeInTheDocument();
    expect(within(mine).getByText("you")).toBeInTheDocument();
    const bobs = within(table).getByRole("row", { name: /bobs-ci/ });
    expect(within(bobs).getByText("2 entries")).toHaveAttribute("title", "burrow-simple, ollama/*");
    expect(within(bobs).getByText("bur_usr_bob0002")).toBeInTheDocument();
  });

  it("a non-admin sees only their own keys and no owner column", async () => {
    otherUsersKey();
    mountAs("user");
    const table = await screen.findByRole("table", { name: "Gateway keys" });
    expect(within(table).getByRole("row", { name: /laptop/ })).toBeInTheDocument();
    expect(within(table).queryByRole("row", { name: /bobs-ci/ })).toBeNull();
    expect(within(table).queryByRole("columnheader", { name: "Owner" })).toBeNull();
  });

  it("revokes a key after confirmation", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Gateway keys" });
    await userEvent.click(within(table).getAllByRole("button", { name: /^Revoke / })[0]);
    const dialog = await screen.findByRole("dialog", { name: /revoke/i });
    expect(within(dialog).getByText(/stop working immediately/i)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke key" }));
    await waitFor(() => expect(within(table).getByText("revoked")).toBeInTheDocument());
    // A revoked key stays listed and cannot be revoked again.
    expect(within(table).queryByRole("button", { name: /^Revoke / })).toBeNull();
  });

  it("names the other two kinds of credential with a link", async () => {
    mount();
    await screen.findByRole("heading", { name: "Gateway keys" });
    expect(screen.getByRole("link", { name: "Client tokens" })).toHaveAttribute("href", "/clients?tab=tokens");
    expect(screen.getByRole("link", { name: "Automation tokens" })).toHaveAttribute("href", "/settings/automation");
  });

  it("without a key: says what a key is for and offers to create one", async () => {
    db.aiGatewayKeys = [];
    mount();
    expect(await screen.findByText("No gateway keys yet")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("keys failing: an error with a retry", async () => {
    server.use(http.get("/api/v1/ai/keys", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load gateway keys: boom");
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });
});
