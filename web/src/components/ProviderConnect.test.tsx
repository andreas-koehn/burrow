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
    expect(screen.getByRole("region", { name: "Connect a client" })).toBeInTheDocument();
  });
});
