import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db, removeProvider } from "@/mocks/db";
import { NewProviderDialog } from "@/components/NewProviderDialog";

function renderDialog() {
  // The presets for z.ai create the slug "zai": the seeded provider of that name is not there yet.
  removeProvider("zai");
  const onOpenChange = vi.fn();
  renderApp(<NewProviderDialog open onOpenChange={onOpenChange} />, "/gateway/providers");
  return { onOpenChange };
}

// The ds Select is a button with a listbox, not a native <select>.
async function choosePreset(label: string) {
  await userEvent.click(await screen.findByLabelText("Provider"));
  await userEvent.click(await screen.findByRole("option", { name: label }));
}

const RESPONSES = "Offers the Responses API (needed by Codex)";

async function hosted() {
  await userEvent.click(screen.getByRole("radio", { name: /a hosted api/i }));
}

describe("New provider dialog", () => {
  it("starts on a choice between a local service and a hosted API", async () => {
    renderDialog();
    const group = screen.getByRole("radiogroup", { name: "What the provider serves" });
    expect(group).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /a service behind a burrow client/i })).toBeChecked();
    expect(screen.getByRole("radio", { name: /a hosted api/i })).not.toBeChecked();
  });

  it("prefills from the OpenRouter preset and posts a direct provider", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/providers", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ slug: "openrouter" }, { status: 201 });
    }));
    const { onOpenChange } = renderDialog();
    await hosted();
    await choosePreset("OpenRouter");
    expect(screen.getByLabelText("Name")).toHaveValue("OpenRouter");
    expect(screen.getByLabelText("Base URL")).toHaveValue("https://openrouter.ai/api/v1");
    expect(screen.getByLabelText("Provider slug")).toHaveValue("openrouter");
    expect(screen.getByLabelText("Credential slot")).toHaveValue("OPENROUTER");
    expect(screen.getByText("Cost is taken from what OpenRouter reports per request.")).toBeInTheDocument();
    expect(screen.queryByText(/BURROW_UPSTREAM_KEY_OPENROUTER/)).toBeNull(); // slot exists → no warning
    // A slot that exists can still be empty; the field says what counts.
    expect(screen.getByLabelText("Credential slot")).toHaveAccessibleDescription(/must be set to a non-empty value/);
    // OpenRouter documents POST /responses: the preset ticks the box.
    expect(screen.getByRole("checkbox", { name: RESPONSES })).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({
      kind: "direct", slug: "openrouter", name: "OpenRouter",
      base_url: "https://openrouter.ai/api/v1", credential_slot: "OPENROUTER", billing: "metered",
      supports_responses: true,
    }));
    // Only what the API accepts; nothing that could carry a credential.
    expect(Object.keys(posted!).sort()).toEqual(["base_url", "billing", "credential_slot", "kind", "max_concurrent", "name", "slug", "supports_responses"]);
    // No limit unless the operator sets one.
    expect(posted!.max_concurrent).toBe(0);
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("leaves the Responses API off for z.ai, also after OpenRouter was chosen first", async () => {
    const { onOpenChange } = renderDialog();
    // A local service has no such choice in this dialog.
    expect(screen.queryByRole("checkbox", { name: RESPONSES })).toBeNull();
    await hosted();
    const box = screen.getByRole("checkbox", { name: RESPONSES });
    expect(box).not.toBeChecked();
    expect(box).toHaveAccessibleDescription(
      "Leave off unless the provider documents POST /responses. While off, /openai/v1/responses refuses this provider's models.",
    );
    await choosePreset("OpenRouter");
    expect(box).toBeChecked();
    await choosePreset("z.ai — API (pay as you go)");
    expect(box).not.toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "zai", supports_responses: false });
  });

  it("the operator can tick the Responses API for any hosted API", async () => {
    const { onOpenChange } = renderDialog();
    await hosted();
    await choosePreset("z.ai — Coding Plan");
    await userEvent.click(screen.getByRole("checkbox", { name: RESPONSES }));
    expect(screen.getByRole("checkbox", { name: RESPONSES })).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "zai", supports_responses: true });
  });

  it("a hosted API can be given a concurrency limit; a local service gets it on its own page", async () => {
    const { onOpenChange } = renderDialog();
    expect(screen.queryByRole("textbox", { name: "Requests at once" })).toBeNull();
    await hosted();
    const field = screen.getByRole("textbox", { name: "Requests at once" });
    expect(field).toHaveValue("");
    expect(field).toHaveAccessibleDescription(
      "How many requests this provider serves in parallel. More wait for a free place. Leave empty for no limit — set it for a local model on one GPU.",
    );
    await choosePreset("z.ai — Coding Plan");
    await userEvent.type(field, "2");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "zai", max_concurrent: 2 });
  });

  it("a concurrency limit of 1001 shows the server's reason on the field", async () => {
    const { onOpenChange } = renderDialog();
    await hosted();
    await choosePreset("z.ai — Coding Plan");
    const field = screen.getByRole("textbox", { name: "Requests at once" });
    await userEvent.type(field, "1001");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("max concurrent requests must be between 0 and 1000")).toBeInTheDocument();
    expect(field).toBeInvalid();
    expect(field).toHaveAccessibleDescription("max concurrent requests must be between 0 and 1000");
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(db.aiProviders.some((p) => p.slug === "zai")).toBe(false);
    // Not a whole number: Create is blocked before anything is sent.
    await userEvent.clear(field);
    await userEvent.type(field, "-1");
    expect(screen.getByText("Enter a whole number, or leave empty for no limit.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("creates through the mock API and choosing another preset overwrites the fields", async () => {
    const { onOpenChange } = renderDialog();
    await hosted();
    await choosePreset("OpenRouter");
    await choosePreset("z.ai — Coding Plan");
    expect(screen.getByLabelText("Name")).toHaveValue("z.ai");
    expect(screen.getByLabelText("Provider slug")).toHaveValue("zai");
    expect(screen.getByLabelText("Base URL")).toHaveValue("https://api.z.ai/api/coding/paas/v4");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(db.aiProviders.at(-1)).toMatchObject({
      slug: "zai", kind: "direct", upstream_base_url: "https://api.z.ai/api/coding/paas/v4", credential_slot: "ZAI", billing: "flat",
    });
  });

  it("tells the admin which variable to set when the slot is missing, and still allows creating", async () => {
    renderDialog();
    await hosted();
    await choosePreset("z.ai — Coding Plan");
    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI");
    expect(note).toHaveTextContent(/restart/i);
    // Same condition as the "not configured" badge later: set and non-empty.
    expect(note).toHaveTextContent("Slot ZAI is not set on the relay.");
    expect(note).toHaveTextContent(/to a non-empty value/);
    expect(screen.getByRole("button", { name: "Create" })).toBeEnabled();
  });

  it("takes several slots, posts them joined and names only the missing one", async () => {
    db.upstreamSlots.push("ZAI");
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/providers", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ slug: "z-ai" }, { status: 201 });
    }));
    renderDialog();
    await hosted();
    await choosePreset("z.ai — Coding Plan");
    const slot = screen.getByLabelText("Credential slot");
    expect(slot).toHaveAccessibleDescription(/One slot, or up to four separated by commas — tried in order\./);
    await userEvent.clear(slot);
    await userEvent.type(slot, "zai, zai2");
    expect(slot).toHaveValue("ZAI, ZAI2");
    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent("Slot ZAI2 is not set on the relay.");
    expect(note).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI2");
    expect(note).not.toHaveTextContent(/BURROW_UPSTREAM_KEY_ZAI\b(?!2)/);
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({ credential_slot: "ZAI,ZAI2" }));
  });

  it("names every missing slot of several", async () => {
    renderDialog();
    await hosted();
    await choosePreset("z.ai — Coding Plan");
    await userEvent.type(screen.getByLabelText("Credential slot"), ",zai2");
    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent("Slots ZAI and ZAI2 are not set on the relay.");
    expect(note).toHaveTextContent("BURROW_UPSTREAM_KEY_ZAI and BURROW_UPSTREAM_KEY_ZAI2");
  });

  it("refuses more than four slots and a slot listed twice", async () => {
    renderDialog();
    await hosted();
    const slot = screen.getByLabelText("Credential slot");
    await userEvent.type(slot, "A,B,C,D,E");
    expect(slot).toHaveAccessibleDescription(/At most four slots\./);
    await userEvent.clear(slot);
    await userEvent.type(slot, "A,B,A");
    expect(slot).toHaveAccessibleDescription(/Slot A is listed twice\./);
    expect(slot).toHaveAttribute("aria-invalid", "true");
  });

  it("never offers a field for the key itself", async () => {
    renderDialog();
    await hosted();
    expect(screen.queryByLabelText(/api key|secret|token/i)).toBeNull();
    expect(document.querySelector('input[type="password"]')).toBeNull();
  });

  it("uppercases the credential slot as typed and rejects characters a slot cannot have", async () => {
    renderDialog();
    await hosted();
    const slot = screen.getByLabelText("Credential slot");
    await userEvent.type(slot, "my_slot");
    expect(slot).toHaveValue("MY_SLOT");
    await userEvent.type(slot, "-x");
    expect(slot).toHaveAttribute("aria-invalid", "true");
    expect(slot).toHaveAccessibleDescription(/A–Z, 0–9 and underscore/);
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  });

  it("shows the server's reason when the base URL is refused, on the base URL field", async () => {
    renderDialog();
    await hosted();
    await choosePreset("Other OpenAI-compatible API");
    await userEvent.type(screen.getByLabelText("Name"), "LAN");
    await userEvent.type(screen.getByLabelText("Provider slug"), "lan");
    await userEvent.type(screen.getByLabelText("Base URL"), "http://10.0.0.5/v1");
    await userEvent.type(screen.getByLabelText("Credential slot"), "LAN");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/https URL/);
    const url = screen.getByLabelText("Base URL");
    expect(url).toHaveAttribute("aria-invalid", "true");
    expect(url).toHaveAccessibleDescription(/https URL/);
    // Editing the field clears the server's verdict.
    await userEvent.type(url, "x");
    expect(url).not.toHaveAttribute("aria-invalid");
  });

  it("reports a taken slug on the slug field", async () => {
    addDirectProvider("openrouter");
    renderDialog();
    await hosted();
    await choosePreset("OpenRouter");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    const slug = screen.getByLabelText("Provider slug");
    await waitFor(() => expect(slug).toHaveAttribute("aria-invalid", "true"));
    expect(slug).toHaveAccessibleDescription("provider slug or service already in use");
  });

  it("shows any other refusal in the dialog body", async () => {
    server.use(http.post("/api/v1/ai/providers", () =>
      HttpResponse.json({ error: "auth header is not a valid header name" }, { status: 400 })));
    renderDialog();
    await hosted();
    await choosePreset("OpenRouter");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("auth header is not a valid header name");
    expect(screen.getByLabelText("Provider slug")).not.toHaveAttribute("aria-invalid");
  });

  it("switching back to a local service restores the service form", async () => {
    renderDialog();
    await hosted();
    expect(screen.queryByText(/no eligible service/i)).toBeNull();
    await userEvent.click(screen.getByRole("radio", { name: /a service behind a burrow client/i }));
    expect(await screen.findByText(/no eligible service/i)).toBeInTheDocument();
    expect(screen.queryByLabelText("Base URL")).toBeNull();
  });

  it("rejects a credential slot that ends in _FILE", async () => {
    renderDialog();
    await hosted();
    await choosePreset("OpenRouter");
    const slot = screen.getByLabelText("Credential slot");
    await userEvent.clear(slot);
    await userEvent.type(slot, "foo_file");
    expect(slot).toHaveAttribute("aria-invalid", "true");
    expect(slot).toHaveAccessibleDescription(/cannot end in _FILE/);
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    expect(screen.queryByRole("note")).toBeNull();
  });
});
