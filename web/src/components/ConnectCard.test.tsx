import { describe, it, expect, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { ConnectCard, type ConnectModel } from "@/components/ConnectCard";

const endpoints = [
  { dialect: "openai" as const, base_url: "https://b.example.com/openai/v1" },
  { dialect: "anthropic" as const, base_url: "https://b.example.com/anthropic" },
];
const native: ConnectModel = {
  name: "burrow-intelligence", dialect_modes: { openai: "native", anthropic: "native" }, responses_mode: "native", translation_pairs: {},
};
/** An OpenAI target only, translation off. */
const openaiOnly: ConnectModel = {
  name: "burrow-simple", dialect_modes: { openai: "native", anthropic: "not_served" }, responses_mode: "native", translation_pairs: {},
};
/** An OpenAI target without the Responses API, translation on. */
const translated: ConnectModel = {
  name: "burrow-simple", dialect_modes: { openai: "native", anthropic: "translated" }, responses_mode: "translated",
  translation_pairs: { anthropic: "messages-chat", responses: "responses-chat" },
};
/** An Anthropic target only, translation on. */
const anthropicOnly: ConnectModel = {
  name: "burrow-claude", dialect_modes: { openai: "translated", anthropic: "native" }, responses_mode: "translated",
  translation_pairs: { openai: "chat-messages", responses: "responses-messages" },
};
const models: ConnectModel[] = [native, openaiOnly];
const offered = (name: string) => within(screen.getByRole("list", { name })).getAllByRole("listitem").map((li) => li.textContent);

function mount(list = models) {
  return renderApp(<ConnectCard endpoints={endpoints} models={list} />, "/gateway/models");
}

function clipboard() {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  return writeText;
}

describe("ConnectCard", () => {
  it("has one tab per client and starts on Claude Code", () => {
    mount();
    expect(screen.getByRole("heading", { name: "Connect a client" })).toBeInTheDocument();
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Claude Code", "Codex", "OpenAI SDK", "Other"]);
    expect(screen.getByRole("tab", { name: "Claude Code" })).toHaveAttribute("aria-selected", "true");
  });

  it("Claude Code: shows the anthropic base URL, lists only models served in that format, and says what is left out", async () => {
    mount();
    // After userEvent set up its own clipboard stub.
    const user = userEvent.setup();
    const writeText = clipboard();
    expect(screen.getByText("https://b.example.com/anthropic")).toBeInTheDocument();
    expect(screen.getByText("1 model is not served in the Anthropic format: burrow-simple")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy settings for Claude Code" }));
    const env = JSON.parse(writeText.mock.calls[0][0]).env;
    expect(env.ANTHROPIC_BASE_URL).toBe("https://b.example.com/anthropic");
    expect(env.ANTHROPIC_DEFAULT_SONNET_MODEL).toBe("burrow-intelligence");
    expect(Object.values(env)).not.toContain("burrow-simple");
    // The key is a placeholder to replace, never a key.
    expect(env.ANTHROPIC_AUTH_TOKEN).toBe("<your gateway key>");
  });

  it("Claude Code: names several left-out models in the plural", () => {
    mount([...models, { ...openaiOnly, name: "burrow-medium" }]);
    expect(screen.getByText("2 models are not served in the Anthropic format: burrow-simple, burrow-medium")).toBeInTheDocument();
  });

  it("Codex: shows the openai base URL and a config.toml block", async () => {
    mount();
    await userEvent.click(screen.getByRole("tab", { name: "Codex" }));
    expect(screen.getByText("https://b.example.com/openai/v1")).toBeInTheDocument();
    expect(screen.getByText(/wire_api = "responses"/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy config for Codex" })).toBeInTheDocument();
    expect(screen.getByText(/Codex speaks the Responses API/i)).toBeInTheDocument();
  });

  it("lists translated models with a mark and explains it", async () => {
    mount([translated, native]);
    const user = userEvent.setup();
    const writeText = clipboard();
    // Both are offered now, the natively served one first; the translated one says so in words.
    expect(offered("Models for Claude Code")).toEqual(["burrow-intelligence", "burrow-simpletranslated"]);
    expect(screen.queryByText(/not served in the Anthropic format/)).toBeNull();
    expect(screen.getByText(/Translated models run without extended thinking; these settings turn it off/)).toBeInTheDocument();
    expect(screen.getByText(/prompt caching hints, thinking signatures and beta features are not carried over/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy settings for Claude Code" }));
    const env = JSON.parse(writeText.mock.calls[0][0]).env;
    expect(env.ANTHROPIC_DEFAULT_OPUS_MODEL).toBe("burrow-intelligence");
    expect(env.ANTHROPIC_DEFAULT_SONNET_MODEL).toBe("burrow-simple");
    expect(env.MAX_THINKING_TOKENS).toBe("0");
  });

  it("Claude Code: leaves thinking alone when no pinned model is translated", async () => {
    mount();
    const user = userEvent.setup();
    const writeText = clipboard();
    expect(screen.queryByText(/extended thinking/)).toBeNull();
    expect(offered("Models for Claude Code")).toEqual(["burrow-intelligence"]);
    await user.click(screen.getByRole("button", { name: "Copy settings for Claude Code" }));
    expect("MAX_THINKING_TOKENS" in JSON.parse(writeText.mock.calls[0][0]).env).toBe(false);
  });

  it("Codex: offers what the Responses API serves, natively or translated, and nothing else", async () => {
    mount([{ ...openaiOnly, name: "burrow-chat-only", responses_mode: "not_served" }, translated, native]);
    await userEvent.click(screen.getByRole("tab", { name: "Codex" }));
    expect(offered("Models for Codex")).toEqual(["burrow-intelligence", "burrow-simpletranslated"]);
    // The config names a natively served model first.
    expect(screen.getByText(/model = "burrow-intelligence"/)).toBeInTheDocument();
    expect(screen.getByText("1 model is not served on the Responses API: burrow-chat-only")).toBeInTheDocument();
    expect(screen.getByText(/keeps no response state/i)).toBeInTheDocument();
    // No Anthropic-format provider is involved here.
    expect(screen.queryByText(/32,000/)).toBeNull();
  });

  it("Codex: says what a model behind an Anthropic-format provider loses", async () => {
    mount([anthropicOnly]);
    await userEvent.click(screen.getByRole("tab", { name: "Codex" }));
    expect(offered("Models for Codex")).toEqual(["burrow-claudetranslated"]);
    expect(screen.getByText(/output is capped at 32,000 tokens when the client sets no limit, and temperature and top_p are not sent/i)).toBeInTheDocument();
  });

  it("Codex: explains what to do when no model is served on the Responses API", async () => {
    mount([{ ...openaiOnly, responses_mode: "not_served" }]);
    await userEvent.click(screen.getByRole("tab", { name: "Codex" }));
    expect(screen.getByText(/No model is served on the Responses API yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Copy config for Codex" })).toBeNull();
  });

  it("OpenAI SDK and Other: a model translated into the OpenAI format is offered and marked", async () => {
    mount([anthropicOnly]);
    await userEvent.click(screen.getByRole("tab", { name: "OpenAI SDK" }));
    expect(offered("Models for the OpenAI SDK")).toEqual(["burrow-claudetranslated"]);
    expect(screen.getAllByText(/model="burrow-claude"/).length).toBeGreaterThan(0);
    await userEvent.click(screen.getByRole("tab", { name: "Other" }));
    expect(offered("Models for other clients")).toEqual(["burrow-claudetranslated"]);
  });

  it("OpenAI SDK: offers Python and TypeScript", async () => {
    mount();
    await userEvent.click(screen.getByRole("tab", { name: "OpenAI SDK" }));
    expect(screen.getByRole("button", { name: "Copy Python example" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy TypeScript example" })).toBeInTheDocument();
  });

  it("Other: lists the three values of a generic provider form, each copyable", async () => {
    mount();
    const user = userEvent.setup();
    await user.click(screen.getByRole("tab", { name: "Other" }));
    for (const label of ["Base URL", "API key", "Models"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: `Copy ${label}` })).toBeInTheDocument();
    }
    expect(screen.getByText("burrow-intelligence, burrow-simple")).toBeInTheDocument();
    const writeText = clipboard();
    await user.click(screen.getByRole("button", { name: "Copy Base URL" }));
    expect(writeText).toHaveBeenCalledWith("https://b.example.com/openai/v1");
  });

  it("explains what to do when a format has no model yet", () => {
    mount([openaiOnly]);
    expect(screen.getByText(/no model is served in the Anthropic format yet/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Models" })).toHaveAttribute("href", "/gateway/models");
    expect(screen.queryByRole("button", { name: "Copy settings for Claude Code" })).toBeNull();
  });

  it("a snippet's scrolling box is a named group the keyboard can reach", () => {
    mount();
    const box = screen.getByRole("group", { name: "Settings for Claude Code" });
    expect(box).toHaveAttribute("tabindex", "0");
    expect(box).toHaveTextContent("ANTHROPIC_BASE_URL");
  });

  it("tells someone who cannot create a model whom to ask", () => {
    renderApp(<ConnectCard endpoints={endpoints} models={[]} canCreate={false} />, "/gateway");
    expect(screen.getByText(/ask an administrator to create a model/i)).toBeInTheDocument();
    expect(screen.queryByText(/create a model first/i)).toBeNull();
  });

  it("explains what to do when there are no models at all", () => {
    mount([]);
    expect(screen.getByText(/create a model first/i)).toBeInTheDocument();
    expect(screen.queryByRole("tab")).toBeNull();
  });

  it("falls back to this origin when the relay reports no base URL", () => {
    renderApp(<ConnectCard endpoints={[{ dialect: "anthropic", base_url: "" }]} models={models} />, "/gateway/models");
    expect(screen.getByText(`${window.location.origin}/anthropic`)).toBeInTheDocument();
  });
});
