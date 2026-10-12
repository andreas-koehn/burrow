import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { addDirectProvider, db } from "@/mocks/db";
import type { AiModel } from "@/lib/contract";
import { ModelDialog } from "@/components/ModelDialog";

function renderDialog(props: { model?: AiModel } = {}) {
  const onOpenChange = vi.fn();
  renderApp(<ModelDialog open onOpenChange={onOpenChange} {...props} />, "/gateway/models");
  return { onOpenChange };
}

const fixtureModel = (name: string) => db.aiModels.find((m) => m.name === name)!;

// The ds Select is a button with a listbox, not a native <select>.
async function chooseOption(label: string, option: string, container: HTMLElement) {
  await userEvent.click(await within(container).findByLabelText(label));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}
async function listOptions(label: string, container: HTMLElement) {
  await userEvent.click(await within(container).findByLabelText(label));
  const names = (await screen.findAllByRole("option")).map((o) => o.textContent);
  await userEvent.keyboard("{Escape}");
  return names;
}
const group = (name: string) => screen.findByRole("group", { name });
// The target model is a text field that suggests the provider's catalog.
const modelField = (container: HTMLElement) => within(container).findByLabelText("Target model id");
async function typeModel(id: string, container: HTMLElement) {
  await userEvent.type(await modelField(container), id);
}
/** What a field suggests: [value, label] of each option of its datalist. */
function suggested(field: HTMLElement): [string, string | null][] {
  const list = document.getElementById(field.getAttribute("list") ?? "");
  return Array.from(list?.querySelectorAll("option") ?? []).map((o) => [o.value, o.getAttribute("label")]);
}

// OpenRouter next to the seeded providers: an OpenAI-format provider with a synced model list.
function withOpenRouter() {
  addDirectProvider("openrouter");
  db.aiProviderModels.openrouter = [{ id: "google/gemini-x", display_name: "", context_length: 0, synced_at: "2026-05-19T00:00:00Z" }];
}

/** burrow-simple with a second OpenAI target behind ollama/mistral. */
function twoTargets(): AiModel {
  withOpenRouter();
  const m = fixtureModel("burrow-simple");
  m.targets = [...m.targets, { dialect: "openai", provider: "openrouter", model: "google/gemini-x", available: true }];
  return m;
}

describe("ModelDialog", () => {
  it("creates a model with a target in each format", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/models", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-medium" }, { status: 201 });
    }));
    const { onOpenChange } = renderDialog();
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-medium");
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "zai", openai);
    await typeModel("glm-5.1", openai);
    const anthropic = await group("Anthropic format");
    await chooseOption("Provider", "zai-anthropic", anthropic);
    await typeModel("glm-5.1", anthropic);
    await userEvent.click(screen.getByRole("button", { name: "Create model" }));
    await waitFor(() => expect(posted).toMatchObject({
      name: "burrow-medium", enabled: true,
      targets: [
        { dialect: "openai", provider: "zai", model: "glm-5.1" },
        { dialect: "anthropic", provider: "zai-anthropic", model: "glm-5.1" },
      ],
    }));
    // The server derives the formats; sending them is refused.
    expect(posted).not.toHaveProperty("dialects");
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("offers each format only the providers that speak it", async () => {
    renderDialog();
    expect(await listOptions("Provider", await group("Anthropic format"))).toEqual(["Not served", "zai-anthropic"]);
    expect(await listOptions("Provider", await group("OpenAI format"))).toEqual(["Not served", "ollama", "zai", "llamacpp", "lmstudio", "openai-team"]);
  });

  it("accepts one format and requires at least one", async () => {
    renderDialog();
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-x");
    expect(screen.getByRole("button", { name: "Create model" })).toBeDisabled();
    expect(screen.getByText("Choose a target for at least one format.")).toBeInTheDocument();
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "ollama", openai);
    await typeModel("qwen2.5:0.5b", openai);
    expect(screen.getByRole("button", { name: "Create model" })).toBeEnabled();
    expect(screen.queryByText("Choose a target for at least one format.")).toBeNull();
  });

  it("suggests the provider's catalog: the id is taken, the display name is shown with it", async () => {
    db.aiProviderModels.zai = [
      { id: "glm-5.1", display_name: "GLM 5.1", context_length: 200000, synced_at: "2026-05-19T00:00:00Z" },
      { id: "glm-4", display_name: "", context_length: 0, synced_at: "2026-05-19T00:00:00Z" },
    ];
    renderDialog();
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "zai", openai);
    const field = await modelField(openai);
    expect(within(openai).getByRole("combobox", { name: "Target model id" })).toBe(field);
    expect(suggested(field)).toEqual([["glm-4", null], ["glm-5.1", "GLM 5.1"]]);
    expect(field).toHaveAccessibleDescription("The provider's own id for the model: one from its catalog, or typed.");
    // Nothing to sync while the catalog has entries.
    expect(within(openai).queryByRole("button", { name: "Sync catalog" })).toBeNull();
  });

  it("takes a model id the catalog does not have", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/models", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-free" }, { status: 201 });
    }));
    renderDialog();
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-free");
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "zai", openai);
    await typeModel("glm-9-preview", openai);
    await userEvent.click(screen.getByRole("button", { name: "Create model" }));
    await waitFor(() => expect(posted).toMatchObject({ targets: [{ dialect: "openai", provider: "zai", model: "glm-9-preview" }] }));
  });

  it("shows a stored model id that is not in the catalog any more", async () => {
    const model = fixtureModel("burrow-simple");
    model.targets = [{ dialect: "openai", provider: "ollama", model: "gone:7b", available: false }];
    renderDialog({ model });
    expect(await modelField(await group("OpenAI format"))).toHaveValue("gone:7b");
  });

  it("an empty catalog says so; a hosted provider that is ready can be synced, and the list fills", async () => {
    // openai-team is a hosted provider whose credential is set; nothing was synced yet.
    renderDialog();
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "openai-team", openai);
    const field = await modelField(openai);
    expect(field).toHaveAccessibleDescription("This provider's catalog is empty. Type the model id.");
    expect(suggested(field)).toEqual([]);
    await userEvent.click(within(openai).getByRole("button", { name: "Sync catalog" }));
    await waitFor(() => expect(suggested(field)).toEqual([["acme/large-1", "Acme Large 1"], ["acme/small-1", "Acme Small 1"]]));
    // The wait is over: said to a screen reader.
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Catalog synced: 2 models."));
    expect(field).toHaveAccessibleDescription("The provider's own id for the model: one from its catalog, or typed.");
    expect(within(openai).queryByRole("button", { name: "Sync catalog" })).toBeNull();
  });

  it("offers no sync where the relay cannot read a catalog: a tunnel provider, or a hosted one without its credential", async () => {
    // The relay syncs from a hosted provider's upstream only; llamacpp is a tunnel provider that is offline.
    db.aiProviderModels.ollama = [];
    db.aiProviderModels.zai = [];
    renderDialog();
    const openai = await group("OpenAI format");
    for (const slug of ["llamacpp", "ollama", "zai"]) {
      await chooseOption("Provider", slug, openai);
      expect(await modelField(openai)).toHaveAccessibleDescription("This provider's catalog is empty. Type the model id.");
      expect(within(openai).queryByRole("button", { name: "Sync catalog" })).toBeNull();
    }
  });

  it("a failed sync shows the relay's reason under the field and keeps what was typed", async () => {
    server.use(http.post("/api/v1/ai/providers/openai-team/models/sync", () =>
      HttpResponse.json({ error: "the upstream answered 401" }, { status: 502 })));
    renderDialog();
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "openai-team", openai);
    await typeModel("gpt-x", openai);
    await userEvent.click(within(openai).getByRole("button", { name: "Sync catalog" }));
    expect(await within(openai).findByRole("alert")).toHaveTextContent("the upstream answered 401");
    const field = await modelField(openai);
    expect(field).toHaveValue("gpt-x");
    expect(field).toHaveAccessibleDescription("the upstream answered 401");
    // It can be tried again, and the field still takes what is typed.
    expect(within(openai).getByRole("button", { name: "Sync catalog" })).toBeEnabled();
    await userEvent.type(field, "y");
    expect(field).toHaveValue("gpt-xy");
  });

  it("says so when no provider speaks a format", async () => {
    db.aiProviders = db.aiProviders.filter((p) => p.slug === "ollama");
    renderDialog();
    const anthropic = await group("Anthropic format");
    expect(await within(anthropic).findByText("No provider speaks this format yet.")).toBeInTheDocument();
    expect(within(anthropic).getByRole("link", { name: "Providers" })).toHaveAttribute("href", "/gateway/providers");
  });

  it("blocks invalid names", async () => {
    renderDialog();
    await userEvent.type(screen.getByLabelText("Model name"), "Bad/Name");
    expect(screen.getByRole("alert")).toHaveTextContent(/no slash/i);
    expect(screen.getByRole("button", { name: "Create model" })).toBeDisabled();
  });

  it("shows the server's reason on failure and stays open", async () => {
    const { onOpenChange } = renderDialog();
    // The name of an existing model: the relay answers 409.
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-simple");
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "zai", openai);
    await typeModel("glm-5.1", openai);
    await userEvent.click(screen.getByRole("button", { name: "Create model" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("model name already in use");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("says who may change models when the relay answers 403", async () => {
    server.use(http.post("/api/v1/ai/models", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    renderDialog();
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-y");
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "zai", openai);
    await typeModel("glm-5.1", openai);
    await userEvent.click(screen.getByRole("button", { name: "Create model" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("You don't have permission to change models.");
  });

  it("a model in both formats: Save stays disabled until something changes, whatever order the relay lists the targets in", async () => {
    const model = fixtureModel("burrow-intelligence");
    // As the relay returns them: by format, anthropic first.
    expect(model.targets.map((t) => t.dialect)).toEqual(["anthropic", "openai"]);
    renderDialog({ model });
    const save = screen.getByRole("button", { name: "Save changes" });
    // Both provider lists and both model lists are in.
    expect(await modelField(await group("Anthropic format"))).toHaveValue("glm-5.1");
    expect(await modelField(await group("OpenAI format"))).toHaveValue("glm-5.1");
    expect(save).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Description (optional)"), "x");
    expect(save).toBeEnabled();
    await userEvent.clear(screen.getByLabelText("Description (optional)"));
    expect(save).toBeDisabled();
  });

  it("edits an existing model; Save is disabled until something changes", async () => {
    let put: { url: string; body: Record<string, unknown> } | null = null;
    server.use(http.put("/api/v1/ai/models/:name", async ({ request }) => {
      put = { url: request.url, body: (await request.json()) as Record<string, unknown> };
      return HttpResponse.json({ name: "burrow-simple" });
    }));
    renderDialog({ model: fixtureModel("burrow-simple") });
    expect(screen.getByLabelText("Model name")).toHaveValue("burrow-simple");
    const save = screen.getByRole("button", { name: "Save changes" });
    expect(save).toBeDisabled();
    await userEvent.click(screen.getByRole("switch", { name: "Enabled" }));
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await waitFor(() => expect(put).not.toBeNull());
    expect(put!.url).toMatch(/\/api\/v1\/ai\/models\/burrow-simple$/);
    // What the dialog does not edit goes back as it came.
    expect(put!.body).toEqual({
      name: "burrow-simple", description: "Small and local.", enabled: false, fallback_on_rate_limit: false,
      translate: false, attempt_timeout_s: 60, total_timeout_s: 120,
      targets: [{ dialect: "openai", provider: "ollama", model: "mistral" }],
    });
  });

  it("has a translation switch that says what it does and what is lost", async () => {
    let put: Record<string, unknown> | null = null;
    server.use(http.put("/api/v1/ai/models/:name", async ({ request }) => {
      put = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-simple" });
    }));
    renderDialog({ model: fixtureModel("burrow-simple") }); // an OpenAI target only
    const sw = screen.getByRole("switch", { name: "Translate for the other format" });
    expect(sw).not.toBeChecked();
    // The help is the switch's description, not only text nearby.
    expect(sw).toHaveAccessibleDescription(/served natively where it has a target/i);
    expect(sw).toHaveAccessibleDescription(/a target of its own format is always used first/i);
    expect(sw).toHaveAccessibleDescription(/prompt caching hints, thinking signatures and beta features are not carried over/i);
    expect(sw).toHaveAccessibleDescription(/names what was left out/i);
    // The Responses API can be translated through the model's own OpenAI-format target.
    expect(sw).toHaveAccessibleDescription(/Responses API is answered through an OpenAI-format target that does not offer it/i);
    const save = screen.getByRole("button", { name: "Save changes" });
    expect(save).toBeDisabled();
    await userEvent.click(sw);
    expect(sw).toBeChecked();
    expect(save).toBeEnabled();
    // Back to the stored value: nothing has changed.
    await userEvent.click(sw);
    expect(save).toBeDisabled();
    await userEvent.click(sw);
    await userEvent.click(save);
    await waitFor(() => expect(put).not.toBeNull());
    expect(put!.translate).toBe(true);
  });

  it("shows the stored translation setting and a new model starts with it off", async () => {
    const model = fixtureModel("burrow-simple");
    model.translate = true;
    renderDialog({ model });
    expect(screen.getByRole("switch", { name: "Translate for the other format" })).toBeChecked();
  });

  it("a new model is created with translation off unless it is switched on", async () => {
    let post: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/ai/models", async ({ request }) => {
      post = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-new" }, { status: 201 });
    }));
    renderDialog();
    expect(screen.getByRole("switch", { name: "Translate for the other format" })).not.toBeChecked();
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-new");
    await chooseOption("Provider", "ollama", await group("OpenAI format"));
    await typeModel("mistral", await group("OpenAI format"));
    await userEvent.click(screen.getByRole("button", { name: "Create model" }));
    await waitFor(() => expect(post).not.toBeNull());
    expect(post!.translate).toBe(false);
  });

  // The dialog has one target list per format ("OpenAI format", "Anthropic format").
  // These tests work inside the OpenAI list.
  it("orders several targets and saves them in that order", async () => {
    withOpenRouter();
    let put: Record<string, unknown> | null = null;
    server.use(http.put("/api/v1/ai/models/burrow-simple", async ({ request }) => {
      put = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-simple" });
    }));
    renderDialog({ model: fixtureModel("burrow-simple") }); // one target: ollama/mistral
    await userEvent.click(await screen.findByRole("button", { name: "Add fallback target" }));
    const second = screen.getByRole("group", { name: "OpenAI format, target 2" });
    await chooseOption("Provider", "openrouter", second);
    await typeModel("google/gemini-x", second);
    await userEvent.click(within(second).getByRole("button", { name: "Move OpenAI target 2 up" }));
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put).toMatchObject({
      targets: [{ dialect: "openai", provider: "openrouter", model: "google/gemini-x" }, { dialect: "openai", provider: "ollama", model: "mistral" }],
    }));
    // Only what the relay accepts in a target: the status fields stay out.
    expect((put!.targets as object[]).map((t) => Object.keys(t).sort())).toEqual([
      ["dialect", "model", "provider"], ["dialect", "model", "provider"],
    ]);
    expect(put).not.toHaveProperty("serving");
  });

  it("cannot remove the only target and caps the list at eight", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    expect(screen.queryByRole("button", { name: "Remove OpenAI target 1" })).toBeNull();
    for (let i = 0; i < 7; i++) await userEvent.click(screen.getByRole("button", { name: "Add fallback target" }));
    expect(screen.getByRole("button", { name: "Add fallback target" })).toBeDisabled();
    expect(screen.getAllByRole("group", { name: /^OpenAI format, target \d$/ })).toHaveLength(8);
  });

  it("says in what order the targets are tried", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    expect(within(await group("OpenAI format")).getByText(
      "Tried in this order. Burrow moves on when a target fails before it has started answering.",
    )).toBeInTheDocument();
  });

  it("moves a target with the keyboard, keeps the focus on it and announces the new place", async () => {
    renderDialog({ model: twoTargets() });
    const openai = await group("OpenAI format");
    const first = within(openai).getByRole("group", { name: "OpenAI format, target 1" });
    // The ends offer one direction only.
    expect(within(first).queryByRole("button", { name: "Move OpenAI target 1 up" })).toBeNull();
    expect(within(openai).queryByRole("button", { name: "Move OpenAI target 2 down" })).toBeNull();
    within(first).getByRole("button", { name: "Move OpenAI target 1 down" }).focus();
    await userEvent.keyboard("{Enter}");
    const moved = within(openai).getByRole("group", { name: "OpenAI format, target 2" });
    expect(await modelField(moved)).toHaveValue("mistral");
    expect(moved).toHaveFocus();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("ollama/mistral is now target 2 of 2 in the OpenAI format."));
  });

  it("names the format in every move and remove button, so the two lists do not repeat a name", async () => {
    withOpenRouter();
    const model = fixtureModel("burrow-intelligence");
    model.targets = [
      ...model.targets,
      { dialect: "anthropic", provider: "zai-anthropic", model: "glm-4", available: false },
      { dialect: "openai", provider: "openrouter", model: "google/gemini-x", available: false },
    ];
    renderDialog({ model });
    await group("OpenAI format");
    const names = screen.getAllByRole("button", { name: /^(Move|Remove) / }).map((b) => b.getAttribute("aria-label"));
    expect(names.sort()).toEqual([
      "Move Anthropic target 1 down", "Move Anthropic target 2 up", "Move OpenAI target 1 down", "Move OpenAI target 2 up",
      "Remove Anthropic target 1", "Remove Anthropic target 2", "Remove OpenAI target 1", "Remove OpenAI target 2",
    ]);
  });

  it("announces the same words again: the region is emptied before each announcement", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    const add = await screen.findByRole("button", { name: "Add fallback target" });
    await userEvent.click(add);
    await userEvent.click(add);
    const status = screen.getByRole("status");
    // Two empty entries, removed one after the other from the same place.
    await userEvent.click(screen.getByRole("button", { name: "Remove OpenAI target 2" }));
    await waitFor(() => expect(status).toHaveTextContent("Target 2 removed from the OpenAI format."));
    await userEvent.click(screen.getByRole("button", { name: "Remove OpenAI target 2" }));
    expect(status).toBeEmptyDOMElement();
    await waitFor(() => expect(status).toHaveTextContent("Target 2 removed from the OpenAI format."));
  });

  it("a reorder is a change; back in the stored order nothing has changed", async () => {
    renderDialog({ model: twoTargets() });
    const save = screen.getByRole("button", { name: "Save changes" });
    expect(save).toBeDisabled();
    await userEvent.click(await screen.findByRole("button", { name: "Move OpenAI target 2 up" }));
    expect(save).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Move OpenAI target 2 up" }));
    expect(save).toBeDisabled();
  });

  it("removes a target and announces it", async () => {
    let put: Record<string, unknown> | null = null;
    server.use(http.put("/api/v1/ai/models/burrow-simple", async ({ request }) => {
      put = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-simple" });
    }));
    renderDialog({ model: twoTargets() });
    await userEvent.click(await screen.findByRole("button", { name: "Remove OpenAI target 1" }));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("ollama/mistral removed from the OpenAI format."));
    expect(screen.queryByRole("group", { name: "OpenAI format, target 2" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove OpenAI target 1" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put).toMatchObject({ targets: [{ dialect: "openai", provider: "openrouter", model: "google/gemini-x" }] }));
    expect(put!.targets).toHaveLength(1);
  });

  it("refuses the same target twice in one format", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    await userEvent.click(await screen.findByRole("button", { name: "Add fallback target" }));
    const second = screen.getByRole("group", { name: "OpenAI format, target 2" });
    await chooseOption("Provider", "ollama", second);
    await typeModel("mistral", second);
    expect(screen.getByRole("alert")).toHaveTextContent("A target is listed twice.");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("an unfinished fallback target blocks Save", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    await userEvent.click(await screen.findByRole("button", { name: "Add fallback target" }));
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(screen.getByText("Every target needs a provider and a model. Remove the ones you do not need.")).toBeInTheDocument();
    // A fallback target is removed, not switched to "Not served".
    expect(await listOptions("Provider", screen.getByRole("group", { name: "OpenAI format, target 2" }))).toEqual(["ollama", "zai", "llamacpp", "lmstudio", "openai-team"]);
  });

  it("offers the rate-limit option and the timeouts under Advanced", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    const advanced = screen.getByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByLabelText("Attempt timeout (seconds)")).toBeNull();
    await userEvent.click(advanced);
    expect(advanced).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("checkbox", { name: "Also fall back when a provider rate-limits (429)" })).not.toBeChecked();
    expect(screen.getByLabelText("Attempt timeout (seconds)")).toHaveValue(60);
    expect(screen.getByLabelText("Total timeout (seconds)")).toHaveValue(120);
  });

  it("saves the rate-limit option and the timeouts", async () => {
    let put: Record<string, unknown> | null = null;
    server.use(http.put("/api/v1/ai/models/burrow-simple", async ({ request }) => {
      put = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ name: "burrow-simple" });
    }));
    renderDialog({ model: fixtureModel("burrow-simple") });
    const save = screen.getByRole("button", { name: "Save changes" });
    await userEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Also fall back when a provider rate-limits (429)" }));
    expect(save).toBeEnabled();
    const attempt = screen.getByLabelText("Attempt timeout (seconds)");
    await userEvent.clear(attempt);
    expect(save).toBeDisabled();
    await userEvent.type(attempt, "45");
    await userEvent.click(save);
    await waitFor(() => expect(put).toMatchObject({ fallback_on_rate_limit: true, attempt_timeout_s: 45, total_timeout_s: 120 }));
  });

  it("refuses a total timeout shorter than the attempt timeout", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    await userEvent.click(screen.getByRole("button", { name: "Advanced" }));
    const total = screen.getByLabelText("Total timeout (seconds)");
    await userEvent.clear(total);
    await userEvent.type(total, "30");
    expect(screen.getByRole("alert")).toHaveTextContent(/not be shorter/i);
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("refuses a timeout outside 1-600 seconds", async () => {
    renderDialog({ model: fixtureModel("burrow-simple") });
    await userEvent.click(screen.getByRole("button", { name: "Advanced" }));
    const total = screen.getByLabelText("Total timeout (seconds)");
    await userEvent.clear(total);
    await userEvent.type(total, "601");
    expect(screen.getByRole("alert")).toHaveTextContent("Between 1 and 600 seconds.");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("shows the relay's reason when it refuses a timeout", async () => {
    server.use(http.put("/api/v1/ai/models/burrow-simple", () =>
      HttpResponse.json({ error: "timeouts must be between 1 and 600 seconds" }, { status: 400 })));
    renderDialog({ model: fixtureModel("burrow-simple") });
    await userEvent.click(screen.getByRole("switch", { name: "Enabled" }));
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("timeouts must be between 1 and 600 seconds");
  });
});
