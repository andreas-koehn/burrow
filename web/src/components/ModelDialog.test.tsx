import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
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
    await chooseOption("Target model", "glm-5.1", openai);
    const anthropic = await group("Anthropic format");
    await chooseOption("Provider", "zai-anthropic", anthropic);
    await chooseOption("Target model", "glm-5.1", anthropic);
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
    expect(await listOptions("Provider", await group("OpenAI format"))).toEqual(["Not served", "ollama", "zai"]);
  });

  it("accepts one format and requires at least one", async () => {
    renderDialog();
    await userEvent.type(screen.getByLabelText("Model name"), "burrow-x");
    expect(screen.getByRole("button", { name: "Create model" })).toBeDisabled();
    expect(screen.getByText("Choose a target for at least one format.")).toBeInTheDocument();
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "ollama", openai);
    await userEvent.click(await within(openai).findByRole("button", { name: "Enter a model id instead" }));
    await userEvent.type(within(openai).getByLabelText("Target model id"), "qwen2.5:0.5b");
    expect(screen.getByRole("button", { name: "Create model" })).toBeEnabled();
    expect(screen.queryByText("Choose a target for at least one format.")).toBeNull();
  });

  it("asks for the model id directly when the provider has no synced models", async () => {
    db.aiProviderModels.zai = [];
    renderDialog();
    const openai = await group("OpenAI format");
    await chooseOption("Provider", "zai", openai);
    expect(await within(openai).findByLabelText("Target model id")).toBeInTheDocument();
    expect(within(openai).getByText("This provider has no synced models. Type the model id.")).toBeInTheDocument();
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
    await chooseOption("Target model", "glm-5.1", openai);
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
    await chooseOption("Target model", "glm-5.1", openai);
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
    expect(await within(await group("Anthropic format")).findByLabelText("Target model")).toHaveTextContent("glm-5.1");
    expect(await within(await group("OpenAI format")).findByLabelText("Target model")).toHaveTextContent("glm-5.1");
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
      attempt_timeout_s: 60, total_timeout_s: 120,
      targets: [{ dialect: "openai", provider: "ollama", model: "mistral" }],
    });
  });
});
