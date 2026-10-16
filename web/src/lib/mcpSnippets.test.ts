import { describe, it, expect } from "vitest";
import { mcpSnippets } from "./mcpSnippets";

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
