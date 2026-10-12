import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db, removeProvider } from "@/mocks/db";
import { NewProviderDialog } from "@/components/NewProviderDialog";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

function renderDialog() {
  // The presets for z.ai create the slug "zai": the seeded provider of that name is not there yet.
  removeProvider("zai");
  const onOpenChange = vi.fn();
  renderApp(
    <>
      <Routes>
        <Route path="/gateway/providers" element={<NewProviderDialog open onOpenChange={onOpenChange} />} />
        <Route path="*" element={null} />
      </Routes>
      <Where />
    </>,
    "/gateway/providers",
  );
  return { onOpenChange };
}

const LOCAL = "A model server on one of my machines";
const HOSTED = "A hosted API";
const useExisting = () => userEvent.click(screen.getByRole("button", { name: "Use a service that already exists" }));

// The ds Select is a button with a listbox, not a native <select>.
async function choosePreset(label: string) {
  await userEvent.click(await screen.findByLabelText("Provider"));
  await userEvent.click(await screen.findByRole("option", { name: label }));
}

const RESPONSES = "Offers the Responses API (needed by Codex)";

/** The lines under the "Credential slot" field: one per name typed. */
const slotLines = async () =>
  within(await screen.findByRole("list", { name: "Credential slots on the relay" })).getAllByRole("listitem").map((li) => li.textContent);
/** The names the "Credential slot" field suggests. */
function slotsOffered(): string[] {
  const list = document.getElementById(screen.getByLabelText("Credential slot").getAttribute("list") ?? "");
  return Array.from(list?.querySelectorAll("option") ?? []).map((o) => o.value);
}
const notSet = (slot: string) => `${slot} is not set. Add BURROW_UPSTREAM_KEY_${slot} to the relay's environment and restart it.`;

async function hosted() {
  await userEvent.click(screen.getByRole("radio", { name: /a hosted api/i }));
}

describe("New provider dialog", () => {
  it("starts on a choice between a model server on one's own machine and a hosted API", async () => {
    renderDialog();
    const group = screen.getByRole("radiogroup", { name: "What the provider serves" });
    // The two choices are named by what the reader has, not by a kind of provider.
    expect(within(group).getAllByRole("radio").map((r) => r.closest("label")!.textContent)).toEqual([LOCAL, HOSTED]);
    expect(screen.getByRole("radio", { name: LOCAL })).toBeChecked();
    expect(screen.getByRole("radio", { name: HOSTED })).not.toBeChecked();
    // The first choice opens the guided steps, in this dialog.
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    expect(screen.getByLabelText("Model server")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
    expect(screen.queryByLabelText("Provider")).toBeNull();
    expect(screen.queryByRole("button", { name: "Create" })).toBeNull();
  });

  it("choosing a hosted API shows the preset field and keeps the focus on the choice", async () => {
    renderDialog();
    const hostedRadio = screen.getByRole("radio", { name: HOSTED });
    await userEvent.click(hostedRadio);
    expect(hostedRadio).toBeChecked();
    expect(screen.getByLabelText("Provider")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Model server")).toBeNull();
    // One dialog all along: the radio that was chosen is still the one in focus.
    await new Promise((r) => setTimeout(r, 120));
    expect(hostedRadio).toHaveFocus();
  });

  it("keeps what was typed into the guided steps while the other choice is looked at", async () => {
    renderDialog();
    await userEvent.type(screen.getByLabelText("Name"), "Ollama box");
    await hosted();
    expect(screen.getByLabelText("Name")).toHaveValue("");
    await userEvent.click(screen.getByRole("radio", { name: LOCAL }));
    expect(screen.getByLabelText("Name")).toHaveValue("Ollama box");
  });

  it("runs the guided steps to the command and Done opens the provider's page", async () => {
    const { onOpenChange } = renderDialog();
    await userEvent.type(screen.getByLabelText("Name"), "Gemma");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    const heading = await screen.findByRole("heading", { name: "Run this on the machine" });
    const dialog = screen.getByRole("dialog", { name: "New provider" });
    expect(dialog).toContainElement(heading);
    expect(within(dialog).getByRole("group", { name: "Run command" })).toHaveTextContent("burrow http 11434 --name gemma");
    // The provider exists: there is nothing left to choose, and nothing to cancel.
    expect(screen.queryByRole("radiogroup", { name: "What the provider serves" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(db.aiProviders.at(-1)).toMatchObject({ slug: "gemma", kind: "tunnel", service_id: "gemma" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(screen.getByTestId("where")).toHaveTextContent("/gateway/providers/gemma");
  });

  it("Cancel in the guided steps closes the dialog and creates nothing", async () => {
    const { onOpenChange } = renderDialog();
    const before = db.services.length;
    await userEvent.type(screen.getByLabelText("Name"), "Gemma");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(db.services).toHaveLength(before);
  });

  it("a service that already exists is chosen below the two choices, and the way back is there", async () => {
    // A second API-key service that backs no provider yet.
    db.services.push({ ...db.services.find((x) => x.id === "svc_ai001")!, id: "svc_ai002", name: "vllm", slug: "vl9k2p" });
    renderDialog();
    await useExisting();
    // The first choice stays chosen; its other form takes the place of the guided steps.
    expect(screen.getByRole("radio", { name: LOCAL })).toBeChecked();
    const name = screen.getByLabelText("Name");
    await waitFor(() => expect(name).toHaveFocus());
    expect(screen.getByLabelText("Provider slug")).toBeInTheDocument();
    expect(await screen.findByLabelText("Service")).toBeInTheDocument();
    expect(screen.queryByLabelText("Model server")).toBeNull();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Set up a model server step by step" }));
    expect(screen.getByLabelText("Model server")).toBeInTheDocument();
    expect(screen.queryByLabelText("Service")).toBeNull();
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
  });

  it("without a service to choose says which one could be, and sends nobody to the Services page", async () => {
    renderDialog();
    await useExisting();
    const note = await screen.findByText("No service can be used: it has to be an HTTP service in API-key mode that is not a provider yet.");
    expect(note.closest("[role=status]")).not.toBeNull();
    const dialog = screen.getByRole("dialog", { name: "New provider" });
    expect(within(dialog).queryByRole("link")).toBeNull();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Set up a model server step by step" })).toBeInTheDocument();
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
    // The slot is one of those set on the relay: nothing to add.
    expect(await slotLines()).toEqual(["OPENROUTER is set on the relay."]);
    expect(screen.queryByText(/BURROW_UPSTREAM_KEY_OPENROUTER/)).toBeNull();
    expect(screen.getByLabelText("API format")).toHaveTextContent("OpenAI");
    // A slot that exists can still be empty; the field says what counts.
    expect(screen.getByLabelText("Credential slot")).toHaveAccessibleDescription(/must be set to a non-empty value/);
    // OpenRouter documents POST /responses: the preset ticks the box.
    expect(screen.getByRole("checkbox", { name: RESPONSES })).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({
      kind: "direct", slug: "openrouter", name: "OpenRouter",
      base_url: "https://openrouter.ai/api/v1", credential_slot: "OPENROUTER", billing: "metered",
      supports_responses: true, api_format: "openai",
    }));
    // Only what the API accepts; nothing that could carry a credential.
    expect(Object.keys(posted!).sort()).toEqual(["api_format", "base_url", "billing", "credential_slot", "kind", "max_concurrent", "name", "slug", "supports_responses"]);
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
    expect(await slotLines()).toEqual([notSet("ZAI")]);
    // The variable's name can be copied as it stands.
    expect(screen.getByRole("button", { name: "Copy BURROW_UPSTREAM_KEY_ZAI" })).toBeInTheDocument();
    // What the key has to be is said with the field.
    expect(screen.getByLabelText("Credential slot")).toHaveAccessibleDescription(/must be set to a non-empty value/);
    expect(screen.getByText("You can create the provider now. It shows as Not configured until the key is set.")).toBeInTheDocument();
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
    // One of each: the first is set, the second is not.
    expect(await slotLines()).toEqual(["ZAI is set on the relay.", notSet("ZAI2")]);
    expect(screen.getAllByRole("button", { name: /^Copy BURROW_UPSTREAM_KEY_/ })).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({ credential_slot: "ZAI,ZAI2" }));
  });

  it("names every missing slot of several", async () => {
    renderDialog();
    await hosted();
    await choosePreset("z.ai — Coding Plan");
    await userEvent.type(screen.getByLabelText("Credential slot"), ",zai2");
    expect(await slotLines()).toEqual([notSet("ZAI"), notSet("ZAI2")]);
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
    await choosePreset("Other API");
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

  it("switching back from a hosted API restores the form that was left", async () => {
    renderDialog();
    await useExisting();
    await screen.findByText(/No service can be used/);
    await hosted();
    expect(screen.queryByText(/No service can be used/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Set up a model server step by step" })).toBeNull();
    await userEvent.click(screen.getByRole("radio", { name: LOCAL }));
    expect(await screen.findByText(/No service can be used/)).toBeInTheDocument();
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
    // A name the relay cannot have is not looked up.
    expect(screen.queryByRole("list", { name: "Credential slots on the relay" })).toBeNull();
  });

  it("offers the slots set on the relay, and after a comma those not named yet", async () => {
    renderDialog();
    await hosted();
    const slot = screen.getByLabelText("Credential slot");
    await waitFor(() => expect(slotsOffered()).toEqual(["ANTHROPIC_TEAM_A", "OPENAI", "OPENROUTER"]));
    // Nothing typed, nothing to say about it.
    expect(screen.queryByRole("list", { name: "Credential slots on the relay" })).toBeNull();
    await userEvent.type(slot, "openai, ");
    expect(slotsOffered()).toEqual(["OPENAI, ANTHROPIC_TEAM_A", "OPENAI, OPENROUTER"]);
    // A name that is in no list can still be typed.
    await userEvent.type(slot, "mine");
    expect(await slotLines()).toEqual(["OPENAI is set on the relay.", notSet("MINE")]);
  });

  it("without a slot on the relay says so, and what to add for the name typed", async () => {
    db.upstreamSlots = [];
    renderDialog();
    await hosted();
    expect(await screen.findByText("No credential is set on the relay yet.")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Credential slot"), "zai");
    expect(await slotLines()).toEqual([notSet("ZAI")]);
    expect(screen.getByText("No credential is set on the relay yet.")).toBeInTheDocument();
  });

  it("the preset sets the API format, and it stays the reader's to change", async () => {
    renderDialog();
    await hosted();
    const format = screen.getByLabelText("API format");
    expect(format).toHaveTextContent("OpenAI");
    await userEvent.click(format);
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["OpenAI", "Anthropic"]);
    await userEvent.click(screen.getByRole("option", { name: "Anthropic" }));
    expect(format).toHaveTextContent("Anthropic");
    // Every preset there is speaks the OpenAI format: choosing one sets it back.
    await choosePreset("z.ai — Coding Plan");
    expect(format).toHaveTextContent("OpenAI");
  });

  it("Other API in the Anthropic format: sent as such, without the Responses API", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/providers", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ slug: "claude" }, { status: 201 });
    }));
    renderDialog();
    await hosted();
    // Ticked by the preset, then left behind with the format.
    await choosePreset("OpenRouter");
    expect(screen.getByRole("checkbox", { name: RESPONSES })).toBeChecked();
    await choosePreset("Other API");
    await userEvent.click(screen.getByRole("checkbox", { name: RESPONSES }));
    await userEvent.click(screen.getByLabelText("API format"));
    await userEvent.click(await screen.findByRole("option", { name: "Anthropic" }));
    // The Responses API belongs to the OpenAI format.
    expect(screen.queryByRole("checkbox", { name: RESPONSES })).toBeNull();
    await userEvent.type(screen.getByLabelText("Name"), "Claude");
    await userEvent.type(screen.getByLabelText("Base URL"), "https://api.example.net/v1");
    await userEvent.type(screen.getByLabelText("Credential slot"), "OPENAI");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({ kind: "direct", api_format: "anthropic", supports_responses: false }));
  });

  it("describes a hosted API without naming one format", async () => {
    renderDialog();
    await hosted();
    expect(within(screen.getByRole("dialog", { name: "New provider" })).getByText(
      "Serve a hosted API under its own base URL. The relay calls it with a credential set on the relay.",
    )).toBeInTheDocument();
  });
});
