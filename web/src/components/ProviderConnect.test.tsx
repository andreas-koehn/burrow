import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ProviderConnect } from "./ProviderConnect";

const BASE = "https://b.example.com/ai/ollama/v1";

function stubClipboard() {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  return writeText;
}

describe("ProviderConnect", () => {
  it("shows the base URL and copies it", async () => {
    const writeText = stubClipboard();
    render(<ProviderConnect baseUrl={BASE} exampleModel="qwen2.5:0.5b" />);
    expect(screen.getByText(BASE)).toBeInTheDocument();
    // The accessible name carries the URL so the button says what it copies.
    await userEvent.click(screen.getByRole("button", { name: `Copy base URL ${BASE}` }));
    expect(writeText).toHaveBeenCalledWith(BASE);
  });

  it("offers a curl snippet that never contains a real key", async () => {
    const writeText = stubClipboard();
    render(<ProviderConnect baseUrl={BASE} exampleModel="qwen2.5:0.5b" />);
    await userEvent.click(screen.getByRole("button", { name: "Copy curl example" }));
    const snippet = writeText.mock.calls[0][0] as string;
    expect(snippet).toContain(`${BASE}/chat/completions`);
    expect(snippet).toContain("Authorization: Bearer $BURROW_API_KEY");
    expect(snippet).toContain('"model": "qwen2.5:0.5b"');
  });

  it("offers the environment variables an OpenAI SDK reads", async () => {
    const writeText = stubClipboard();
    render(<ProviderConnect baseUrl={BASE} />);
    await userEvent.click(screen.getByRole("button", { name: "Copy environment variables" }));
    const snippet = writeText.mock.calls[0][0] as string;
    expect(snippet).toContain(`OPENAI_BASE_URL=${BASE}`);
    expect(snippet).toContain("OPENAI_API_KEY=$BURROW_API_KEY");
  });

  it("uses a placeholder model when none is known", () => {
    render(<ProviderConnect baseUrl={BASE} />);
    expect(screen.getByText(/"model": "<model>"/)).toBeInTheDocument();
  });

  it("labels its region with the heading", () => {
    render(<ProviderConnect baseUrl={BASE} />);
    expect(screen.getByRole("region", { name: "Use it in an app" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Use it in an app" })).toBeInTheDocument();
  });

  it("says what kind of app uses it, and with which key", () => {
    render(<ProviderConnect baseUrl={BASE} />);
    expect(screen.getByText("Use this as the base URL of any OpenAI-compatible app, with a gateway key.")).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/this provider's API keys/);
  });

  describe("a provider of the Anthropic format", () => {
    // An Anthropic app adds /v1/messages itself.
    const ANTHROPIC_BASE = "https://b.example.com/ai/ollama";

    it("names the Anthropic-compatible app and the base URL such an app takes", async () => {
      const writeText = stubClipboard();
      render(<ProviderConnect baseUrl={BASE} apiFormat="anthropic" exampleModel="claude-local" />);
      expect(screen.getByText("Use this as the base URL of any Anthropic-compatible app, with a gateway key.")).toBeInTheDocument();
      expect(screen.getByText(ANTHROPIC_BASE)).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: `Copy base URL ${ANTHROPIC_BASE}` }));
      expect(writeText).toHaveBeenCalledWith(ANTHROPIC_BASE);
    });

    it("shows the Anthropic snippet of the connect card, not the OpenAI one", async () => {
      const writeText = stubClipboard();
      render(<ProviderConnect baseUrl={BASE} apiFormat="anthropic" exampleModel="claude-local" />);
      await userEvent.click(screen.getByRole("button", { name: "Copy settings for Claude Code" }));
      const settings = JSON.parse(writeText.mock.calls[0][0] as string) as { env: Record<string, string> };
      expect(settings.env).toEqual({
        ANTHROPIC_BASE_URL: ANTHROPIC_BASE,
        // A placeholder: the card is never given a key.
        ANTHROPIC_AUTH_TOKEN: "<your gateway key>",
        ANTHROPIC_DEFAULT_OPUS_MODEL: "claude-local",
        ANTHROPIC_DEFAULT_SONNET_MODEL: "claude-local",
        ANTHROPIC_DEFAULT_HAIKU_MODEL: "claude-local",
      });
      expect(screen.queryByRole("button", { name: "Copy curl example" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Copy environment variables" })).toBeNull();
      expect(document.body).not.toHaveTextContent(/OPENAI_|chat\/completions/);
    });

    it("pins no model while none is known", async () => {
      const writeText = stubClipboard();
      render(<ProviderConnect baseUrl={BASE} apiFormat="anthropic" />);
      await userEvent.click(screen.getByRole("button", { name: "Copy settings for Claude Code" }));
      const settings = JSON.parse(writeText.mock.calls[0][0] as string) as { env: Record<string, string> };
      expect(Object.keys(settings.env)).toEqual(["ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"]);
    });
  });
});
