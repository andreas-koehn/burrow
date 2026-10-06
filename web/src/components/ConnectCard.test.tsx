import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import type { Dialect } from "@/lib/contract";
import { ConnectCard } from "@/components/ConnectCard";

const endpoints = [
  { dialect: "openai" as const, base_url: "https://b.example.com/openai/v1" },
  { dialect: "anthropic" as const, base_url: "https://b.example.com/anthropic" },
];
const models: { name: string; dialects: Dialect[] }[] = [
  { name: "burrow-intelligence", dialects: ["anthropic", "openai"] },
  { name: "burrow-simple", dialects: ["openai"] },
];

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
    mount([...models, { name: "burrow-medium", dialects: ["openai"] }]);
    expect(screen.getByText("2 models are not served in the Anthropic format: burrow-simple, burrow-medium")).toBeInTheDocument();
  });

  it("Codex: shows the openai base URL and a config.toml block", async () => {
    mount();
    await userEvent.click(screen.getByRole("tab", { name: "Codex" }));
    expect(screen.getByText("https://b.example.com/openai/v1")).toBeInTheDocument();
    expect(screen.getByText(/wire_api = "responses"/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy config for Codex" })).toBeInTheDocument();
    expect(screen.getByText(/needs a provider that offers the Responses API/i)).toBeInTheDocument();
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
    mount([{ name: "burrow-simple", dialects: ["openai"] }]);
    expect(screen.getByText(/no model is served in the Anthropic format yet/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Models" })).toHaveAttribute("href", "/gateway/models");
    expect(screen.queryByRole("button", { name: "Copy settings for Claude Code" })).toBeNull();
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
