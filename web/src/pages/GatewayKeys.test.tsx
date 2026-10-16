import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import GatewayKeys from "@/pages/GatewayKeys";
import { USER_PERMISSIONS, ALL_PERMISSIONS } from "@/auth/permissions";

function mount() {
  return renderApp(<GatewayKeys />, "/gateway/keys");
}

function Where() {
  const l = useLocation();
  return <div data-testid="location">{l.pathname + l.search}</div>;
}

function mountAt(route: string) {
  return renderApp(
    <>
      <Routes><Route path="/gateway/keys" element={<GatewayKeys />} /></Routes>
      <Where />
    </>,
    route,
  );
}

function mountAs(role: "admin" | "user") {
  db.me = { ...db.me, role, permissions: role === "admin" ? [...ALL_PERMISSIONS] : [...USER_PERMISSIONS] };
  return mount();
}

const otherUsersKey = () => db.aiGatewayKeys.push({
  id: "gk_bob", name: "bobs-ci", key_prefix: "bgw_Zz9y", user_id: "bur_usr_bob0002", allowed_models: ["burrow-simple", "ollama/*"],
  system_text: "", last_used: null, created_at: "2026-05-11T08:00:00Z", revoked_at: null,
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

  it("says so when the key could not be copied, instead of claiming it was", async () => {
    mount();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await user.type(within(dialog).getByLabelText("Name"), "laptop-4");
    await user.click(within(dialog).getByRole("button", { name: "Create key" }));
    await screen.findByLabelText("Your new key");
    // The browser refuses the write (no permission, or a page without TLS).
    const writeText = vi.fn().mockRejectedValue(new Error("denied"));
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    await user.click(screen.getByRole("button", { name: "Copy key" }));
    expect(await screen.findByText("Couldn't copy. Select the text and copy it yourself.")).toBeInTheDocument();
    expect(screen.queryByText("Copied.")).toBeNull();
    // The key is still there to be copied by hand.
    expect((screen.getByLabelText("Your new key") as HTMLInputElement).value).toMatch(/^bgw_/);
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
    expect(within(dialog).getByRole("button", { name: "Create key" })).toHaveAccessibleDescription("Choose at least one model or provider.");
    // What each kind of entry opens: two short lines.
    const lines = [
      "A model allows that name on /openai/v1 and /anthropic.",
      "Everything from a provider also allows the provider's own address /ai/<provider>/….",
    ];
    const first = await within(dialog).findByText(/^A model allows that name on/);
    expect(first.tagName).toBe("P");
    expect(first.textContent).toBe(lines[0]);
    const second = within(dialog).getByText(/^Everything from a provider also allows/);
    expect(second.tagName).toBe("P");
    expect(second.textContent).toBe(lines[1]);
    expect(second).not.toBe(first);
    expect(within(dialog).queryByText(/A model name allows that model/)).toBeNull();
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "burrow-simple" }));
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Everything from ollama" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await waitFor(() => expect(posted).toMatchObject({ name: "ci", allowed_models: ["burrow-simple", "ollama/*"] }));
  });

  it("?new=1&provider=<slug> opens the dialog limited to everything from that provider", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/keys", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "gk9", name: "laptop", key: "bgw_" + "y".repeat(43), allowed_models: ["llamacpp/*"] }, { status: 201 });
    }));
    mountAt("/gateway/keys?new=1&provider=llamacpp");
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    expect(within(dialog).getByRole("radio", { name: "Only these models" })).toBeChecked();
    expect(await within(dialog).findByRole("checkbox", { name: "Everything from llamacpp" })).toBeChecked();
    // The limit is named where it is chosen.
    expect(within(dialog).getByText(/^Everything from a provider also allows/)).toHaveTextContent("the provider's own address /ai/<provider>/…");
    // Nothing else is ticked.
    expect(within(dialog).getAllByRole("checkbox").filter((c) => (c as HTMLInputElement).checked || c.getAttribute("aria-checked") === "true")).toHaveLength(1);
    expect(screen.getByTestId("location").textContent).toBe("/gateway/keys?new=1&provider=llamacpp");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await waitFor(() => expect(posted).toEqual({ name: "laptop", allowed_models: ["llamacpp/*"] }));
    // The key is shown; the link is used up when that dialog closes.
    await userEvent.click(await screen.findByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("location").textContent).toBe("/gateway/keys");
  });

  // The guided provider offers "Create a model", then "Create a gateway key": the key must work with that model on /openai/v1.
  it("?new=1&provider=<slug> also ticks the models that are served by that provider alone", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/keys", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "gk9", name: "app", key: "bgw_" + "y".repeat(43), allowed_models: [] }, { status: 201 });
    }));
    mountAt("/gateway/keys?new=1&provider=ollama");
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    // burrow-simple sends every request to ollama; burrow-intelligence goes elsewhere.
    expect(await within(dialog).findByRole("checkbox", { name: "burrow-simple" })).toBeChecked();
    expect(within(dialog).getByRole("checkbox", { name: "Everything from ollama" })).toBeChecked();
    expect(within(dialog).getByRole("checkbox", { name: "burrow-intelligence" })).not.toBeChecked();
    await userEvent.type(within(dialog).getByLabelText("Name"), "app");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await waitFor(() => expect(posted).toEqual({ name: "app", allowed_models: ["burrow-simple", "ollama/*"] }));
  });

  it("?new=1&provider=<slug>: a model the reader unticks stays unticked", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/keys", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "gk9", name: "app", key: "bgw_" + "y".repeat(43), allowed_models: [] }, { status: 201 });
    }));
    mountAt("/gateway/keys?new=1&provider=ollama");
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "burrow-simple" }));
    expect(within(dialog).getByRole("checkbox", { name: "burrow-simple" })).not.toBeChecked();
    await userEvent.type(within(dialog).getByLabelText("Name"), "app");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    await waitFor(() => expect(posted).toEqual({ name: "app", allowed_models: ["ollama/*"] }));
  });

  it.each(["Cancel", "Close dialog"])("?new=1&provider=<slug>: closing with %s takes both parameters out of the address", async (name) => {
    mountAt("/gateway/keys?new=1&provider=ollama");
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    await userEvent.click(within(dialog).getByRole("button", { name }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("location").textContent).toBe("/gateway/keys");
    // The button opens the usual dialog afterwards.
    await userEvent.click(screen.getByRole("button", { name: "New key" }));
    expect(within(await screen.findByRole("dialog", { name: "New gateway key" })).getByRole("radio", { name: "All models" })).toBeChecked();
  });

  it("?new=1 alone opens the dialog for all models; a provider that is not there leaves nothing ticked", async () => {
    const first = mountAt("/gateway/keys?new=1");
    expect(within(await screen.findByRole("dialog", { name: "New gateway key" })).getByRole("radio", { name: "All models" })).toBeChecked();
    first.unmount();
    mountAt("/gateway/keys?new=1&provider=gone");
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    expect(within(dialog).getByRole("radio", { name: "Only these models" })).toBeChecked();
    await within(dialog).findByRole("checkbox", { name: "burrow-simple" });
    expect(within(dialog).getByRole("button", { name: "Create key" })).toHaveAccessibleDescription("Enter a name.");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    expect(within(dialog).getByRole("button", { name: "Create key" })).toHaveAccessibleDescription("Choose at least one model or provider.");
    // Said once, beside the button it explains.
    expect(within(dialog).getAllByText(/Choose at least one/)).toHaveLength(1);
    expect(within(dialog).getByRole("button", { name: "Create key" })).toBeDisabled();
  });

  it("opens without an error, and the button says what is missing", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "New key" }));
    const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
    expect(within(dialog).queryByRole("alert")).toBeNull();
    const create = within(dialog).getByRole("button", { name: "Create key" });
    expect(create).toBeDisabled();
    expect(within(dialog).getByRole("status")).toHaveTextContent("Enter a name.");
    expect(create).toHaveAccessibleDescription("Enter a name.");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    expect(create).toBeEnabled();
    expect(within(dialog).queryByRole("status")).toBeNull();
    expect(create).not.toHaveAttribute("aria-describedby");
    // The default state is two things to decide: the name, and what the key may use.
    expect(within(dialog).getAllByRole("textbox")).toHaveLength(1);
    expect(within(dialog).getAllByRole("radiogroup")).toHaveLength(1);
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
    // Last used: how long ago (the date, past 30 days), the exact UTC time on hover.
    const used = mine.querySelector("time")!;
    expect(used).toHaveTextContent("18 May 2026");
    expect(used).toHaveAttribute("title", "18 May 2026, 09:00:00 UTC");
    const bobs = within(table).getByRole("row", { name: /bobs-ci/ });
    // The names are a tooltip for a pointer and text for a screen reader.
    const entries = within(bobs).getByTitle("burrow-simple, ollama/*");
    expect(entries).toHaveTextContent("2 entries: burrow-simple, ollama/*");
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
    const line = screen.getByText(/An app sends a gateway key/);
    expect(line).toHaveTextContent(
      "An app sends a gateway key to the AI gateway. To connect a machine, use a client token; to script the management API, use an automation token.",
    );
    expect(within(line).getByRole("link", { name: "client token" })).toHaveAttribute("href", "/clients?tab=tokens");
    expect(within(line).getByRole("link", { name: "automation token" })).toHaveAttribute("href", "/settings/automation");
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

  describe("gateway keys follow ai:configure and ai:read:any", () => {
    it("a custom role with ai:configure:own and ai:read:own can create and revoke, and sees no owner column", async () => {
      db.me = { ...db.me, role: "operator", permissions: ["ai:configure:own", "ai:read:own"] };
      mount();
      const table = await screen.findByRole("table", { name: "Gateway keys" });
      expect(screen.getByRole("button", { name: "New key" })).toBeInTheDocument();
      expect(within(table).getAllByRole("button", { name: /^Revoke / }).length).toBeGreaterThan(0);
      expect(within(table).queryByRole("columnheader", { name: "Owner" })).toBeNull();
    });

    it("without ai:configure there is no New key and no Revoke, and the Owner column follows ai:read:any", async () => {
      db.me = { ...db.me, role: "operator", permissions: ["ai:read:any"] };
      otherUsersKey();
      mount();
      const table = await screen.findByRole("table", { name: "Gateway keys" });
      expect(screen.queryByRole("button", { name: "New key" })).toBeNull();
      expect(within(table).queryByRole("button", { name: /^Revoke / })).toBeNull();
      expect(within(table).getByRole("columnheader", { name: "Owner" })).toBeInTheDocument();
      expect(screen.queryByText(/admin/i)).toBeNull();
    });
  });

  describe("system text and editing", () => {
    it("create sends system_text", async () => {
      let posted: Record<string, unknown> | null = null;
      server.use(http.post("/api/v1/ai/keys", async ({ request }) => {
        posted = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ id: "gk_n", name: "n", key_prefix: "bgw_x", user_id: db.me.id, allowed_models: [], last_used: null, created_at: "2026-05-10T08:00:00Z", revoked_at: null, system_text: "", key: "bgw_secret" }, { status: 201 });
      }));
      mount();
      await userEvent.click(await screen.findByRole("button", { name: "New key" }));
      const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
      await userEvent.type(within(dialog).getByLabelText("Name"), "ci");
      await userEvent.click(within(dialog).getByRole("button", { name: "System text" }));
      await userEvent.type(within(dialog).getByLabelText("System text"), "Be brief.");
      expect(within(dialog).getByText("9 / 8192 bytes")).toBeInTheDocument();
      expect(within(dialog).getByText("Sent before the app's own system text for every request made with this key.")).toBeInTheDocument();
      await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
      await waitFor(() => expect(posted).not.toBeNull());
      expect(posted).toMatchObject({ name: "ci", system_text: "Be brief." });
    });

    it("create is blocked over 8192 bytes", async () => {
      mount();
      await userEvent.click(await screen.findByRole("button", { name: "New key" }));
      const dialog = await screen.findByRole("dialog", { name: "New gateway key" });
      await userEvent.type(within(dialog).getByLabelText("Name"), "ci");
      await userEvent.click(within(dialog).getByRole("button", { name: "System text" }));
      await userEvent.click(within(dialog).getByLabelText("System text"));
      await userEvent.paste("ä".repeat(4097));
      expect(within(dialog).getByText("Too long: at most 8192 bytes.")).toBeInTheDocument();
      expect(within(dialog).getByRole("button", { name: "Create key" })).toBeDisabled();
    });

    it("marks keys that have a system text", async () => {
      db.aiGatewayKeys[0]!.system_text = "Be brief.";
      otherUsersKey();
      mount();
      const table = await screen.findByRole("table", { name: "Gateway keys" });
      expect(within(within(table).getByRole("row", { name: /laptop/ })).getByText("System text")).toBeInTheDocument();
      expect(within(within(table).getByRole("row", { name: /bobs-ci/ })).queryByText("System text")).toBeNull();
    });

    it("Edit is offered with ai:configure:own or :any, and not without", async () => {
      db.me = { ...db.me, role: "operator", permissions: ["ai:configure:own", "ai:read:own"] };
      const first = mount();
      const table = await screen.findByRole("table", { name: "Gateway keys" });
      expect(within(table).getByRole("button", { name: "Edit laptop" })).toBeInTheDocument();
      first.unmount();
      db.me = { ...db.me, role: "viewer", permissions: ["ai:read:own"] };
      mount();
      const table2 = await screen.findByRole("table", { name: "Gateway keys" });
      expect(within(table2).queryByRole("button", { name: /^Edit / })).toBeNull();
    });

    it("Edit PUTs only the changed fields", async () => {
      db.aiGatewayKeys[0]!.system_text = "Old.";
      const puts: Record<string, unknown>[] = [];
      server.use(http.put("/api/v1/ai/keys/gk_laptop1", async ({ request }) => {
        puts.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({});
      }));
      mount();
      await userEvent.click(await screen.findByRole("button", { name: "Edit laptop" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit laptop" });
      const save = within(dialog).getByRole("button", { name: "Save changes" });
      expect(save).toBeDisabled();
      expect(within(dialog).getByLabelText("System text")).toHaveValue("Old.");
      await userEvent.clear(within(dialog).getByLabelText("System text"));
      await userEvent.click(save);
      await waitFor(() => expect(puts).toHaveLength(1));
      // Cleared text is sent as "", the name is left out.
      expect(puts[0]).toEqual({ system_text: "" });
      await waitFor(() => expect(screen.queryByRole("dialog", { name: "Edit laptop" })).toBeNull());
    });

    it("Edit renames with the name alone", async () => {
      const puts: Record<string, unknown>[] = [];
      server.use(http.put("/api/v1/ai/keys/gk_laptop1", async ({ request }) => {
        puts.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({});
      }));
      mount();
      await userEvent.click(await screen.findByRole("button", { name: "Edit laptop" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit laptop" });
      await userEvent.type(within(dialog).getByLabelText("Name"), "-2");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));
      await waitFor(() => expect(puts).toEqual([{ name: "laptop-2" }]));
    });

    it("a 404 says the key no longer exists", async () => {
      server.use(http.put("/api/v1/ai/keys/gk_laptop1", () => HttpResponse.json({ error: "key not found" }, { status: 404 })));
      mount();
      await userEvent.click(await screen.findByRole("button", { name: "Edit laptop" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit laptop" });
      await userEvent.type(within(dialog).getByLabelText("System text"), "x");
      await userEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));
      expect(await within(dialog).findByText("This key no longer exists.")).toBeInTheDocument();
    });
  });
});
