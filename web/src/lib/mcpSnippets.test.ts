import { describe, it, expect } from "vitest";
import { mcpSnippets, mcpOAuthSnippets, GATEWAY_KEY_CLIENT } from "./mcpSnippets";

const URL_ = "https://b.example.com/mcp/burrow";

describe("mcpSnippets", () => {
  const s = mcpSnippets(URL_);

  it("Claude Code is one command with the token as a variable", () => {
    expect(s.claudeCode).toBe(`claude mcp add --transport http burrow ${URL_} --header "Authorization: Bearer $BURROW_TOKEN"`);
  });

  it("Cursor reads the token from the environment", () => {
    expect(JSON.parse(s.cursor)).toEqual({
      mcpServers: { burrow: { url: URL_, headers: { Authorization: "Bearer ${env:BURROW_TOKEN}" } } },
    });
  });

  it("VS Code asks for the token", () => {
    expect(JSON.parse(s.vscode)).toEqual({
      inputs: [{ type: "promptString", id: "burrow-token", description: "Burrow automation token", password: true }],
      servers: { burrow: { type: "http", url: URL_, headers: { Authorization: "Bearer ${input:burrow-token}" } } },
    });
  });

  it("never holds a token value", () => {
    for (const text of Object.values(s)) expect(text).not.toContain("bua_");
  });
});

describe("gateway snippets", () => {
  const URL2 = "https://b.example.com/mcp";
  it("by URL alone for clients that sign in with OAuth", () => {
    const s = mcpOAuthSnippets(URL2);
    expect(s.claudeCode).toBe(`claude mcp add --transport http burrow-tools ${URL2}`);
    expect(JSON.parse(s.cursor)).toEqual({ mcpServers: { "burrow-tools": { url: URL2 } } });
    expect(JSON.parse(s.vscode)).toEqual({ servers: { "burrow-tools": { type: "http", url: URL2 } } });
  });

  it("with a gateway key as a header, held in a variable", () => {
    const s = mcpSnippets(URL2, GATEWAY_KEY_CLIENT);
    expect(s.claudeCode).toBe(`claude mcp add --transport http burrow-tools ${URL2} --header "Authorization: Bearer $BURROW_GATEWAY_KEY"`);
    expect(JSON.parse(s.cursor).mcpServers["burrow-tools"].headers.Authorization).toBe("Bearer ${env:BURROW_GATEWAY_KEY}");
    expect(JSON.parse(s.vscode).inputs[0]).toMatchObject({ id: "burrow-gateway-key", password: true });
    for (const text of Object.values(s)) expect(text).not.toContain("bgw_");
  });
});
