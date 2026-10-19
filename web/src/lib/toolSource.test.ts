import { describe, it, expect } from "vitest";
import { burrowMcpCommand, burrowYamlSnippet, splitCommand, toolRenameError, toolSlugError, toolSlugFromName } from "./toolSource";

describe("toolSlugError", () => {
  it("follows the relay's rule", () => {
    expect(toolSlugError("github")).toBeNull();
    expect(toolSlugError("a")).toBeNull();
    expect(toolSlugError("my-files-2")).toBeNull();
    expect(toolSlugError("")).not.toBeNull();
    expect(toolSlugError("-x")).not.toBeNull();
    expect(toolSlugError("x-")).not.toBeNull();
    expect(toolSlugError("Upper")).not.toBeNull();
    expect(toolSlugError("a".repeat(25))).not.toBeNull();
    expect(toolSlugError("burrow")).toMatch(/reserved/);
    expect(toolSlugError("v1")).toMatch(/reserved/);
  });
  it("suggests a slug from a name", () => {
    expect(toolSlugFromName("My GitHub Tools!")).toBe("my-github-tools");
    expect(toolSlugFromName("x".repeat(40))).toHaveLength(24);
  });
});

describe("toolRenameError", () => {
  it("keeps <slug>__<name> within 64 characters", () => {
    expect(toolRenameError("github", "")).toBeNull();
    expect(toolRenameError("github", "create_issue")).toBeNull();
    expect(toolRenameError("github", "bad name")).not.toBeNull();
    expect(toolRenameError("github", "x".repeat(57))).not.toBeNull();
    expect(toolRenameError("github", "x".repeat(56))).toBeNull();
  });
});

describe("burrow mcp text", () => {
  it("splits a command line with quotes", () => {
    expect(splitCommand(`npx -y pkg "/my dir" ''`)).toEqual(["npx", "-y", "pkg", "/my dir", ""]);
  });
  it("makes the command for a program", () => {
    expect(burrowMcpCommand("files", "npx -y @modelcontextprotocol/server-filesystem /srv"))
      .toBe("burrow mcp files -- npx -y @modelcontextprotocol/server-filesystem /srv");
    expect(burrowMcpCommand("files", `node server.js "my dir"`)).toBe("burrow mcp files -- node server.js 'my dir'");
  });
  it("makes the command for a local address", () => {
    expect(burrowMcpCommand("notes", "http://localhost:3001/mcp")).toBe("burrow mcp notes http://localhost:3001/mcp");
  });
  it("makes the burrow.yaml service", () => {
    expect(burrowYamlSnippet("files", "npx -y pkg /srv")).toBe(
      'services:\n  - name: files\n    type: mcp\n    command: "npx"\n    args: ["-y", "pkg", "/srv"]\n',
    );
    expect(burrowYamlSnippet("notes", "http://localhost:3001/mcp")).toBe(
      'services:\n  - name: notes\n    type: mcp\n    url: "http://localhost:3001/mcp"\n',
    );
  });
});
