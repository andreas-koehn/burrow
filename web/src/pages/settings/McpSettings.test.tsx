import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { ALL_PERMISSIONS } from "@/auth/permissions";
import McpSettings from "@/pages/settings/McpSettings";
import { firstSentence } from "@/lib/firstSentence";

const URL_ = () => `${window.location.origin}/mcp/burrow`;
const mount = () => renderApp(<McpSettings />, "/settings/mcp");

describe("firstSentence", () => {
  it("ends a summary without a full stop at its paragraph", () => {
    expect(firstSentence("List AI providers\n\nReturns one entry per row of the providers table.")).toBe("List AI providers");
  });
  it("takes the first sentence of a paragraph, across line breaks", () => {
    expect(firstSentence("Who this token acts for: the user,\nthe role. And more.")).toBe("Who this token acts for: the user, the role.");
  });
  it("keeps a text without any stop whole", () => {
    expect(firstSentence("Service detail (config + live state)")).toBe("Service detail (config + live state)");
  });
});

describe("Settings → MCP", () => {
  it("shows the address built from the page's origin and a Copy button", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Address" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Endpoint" })).toBeNull();
    expect(await screen.findByText(URL_())).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /copy/i }).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: /copy address/i })).toBeInTheDocument();
  });

  it("offers the three clients as tabs; Cursor shows its file and snippet", async () => {
    mount();
    const tabs = await screen.findAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["Claude Code", "Cursor", "VS Code"]);
    expect(screen.getByText(/claude mcp add --transport http burrow/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Cursor" }));
    expect(screen.getByText("~/.cursor/mcp.json")).toBeInTheDocument();
    expect(screen.getByText(/mcpServers/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "VS Code" }));
    expect(screen.getByText(".vscode/mcp.json")).toBeInTheDocument();
  });

  it("links to a read-only token", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "Create a read-only token" });
    expect(link).toHaveAttribute("href", "/settings/automation?new=1&preset=read-only");
  });

  it("offers no token link to someone who may not make automation tokens, and says what is missing", async () => {
    db.me = { ...db.me, role: "operator-e2e", permissions: ["ai:read:any", "mcp:tools:read"] };
    mount();
    await screen.findByText(/claude mcp add --transport http burrow/);
    expect(screen.queryByRole("link", { name: "Create a read-only token" })).toBeNull();
    expect(screen.getByText(/needs the permission automation:tokens:manage:own/)).toBeInTheDocument();
  });

  it("lists every tool with its kind and what it needs, and the operation count", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "MCP tools" });
    for (const t of db.mcpTools) expect(within(table).getByText(t.name)).toBeInTheDocument();
    const list = within(table).getByText("services_list").closest("tr")!;
    expect(list).toHaveTextContent("Read");
    expect(list).toHaveTextContent("List services");
    expect(list).toHaveTextContent("services:configure:own or services:configure:any");
    expect(within(table).getByText("services_delete").closest("tr")).toHaveTextContent("Delete");
    const generic = within(table).getByText("burrow_call_operation").closest("tr")!;
    expect(generic).toHaveTextContent("Write");
    expect(generic).toHaveTextContent("—");
    expect(screen.getByText(/reach 131 operations in all/)).toBeInTheDocument();
  });

  it("leaves out the sentence when the relay does not count operations", async () => {
    db.mcpStatus = { enabled: true, endpoint: "/mcp/burrow" };
    mount();
    await screen.findByRole("table", { name: "MCP tools" });
    expect(screen.queryByText(/operations in all/)).toBeNull();
  });

  it("shows the recent calls with audit:read", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Recent MCP calls" });
    await waitFor(() => expect(within(table).getByText("services_list")).toBeInTheDocument());
    expect(within(table).getByText("burrow_call_operation/servicesDelete").closest("tr")).toHaveTextContent("denied");
  });

  it("leaves out the recent calls, and asks nothing, without audit:read", async () => {
    db.me = { ...db.me, role: "auditor", permissions: ALL_PERMISSIONS.filter((p) => p !== "audit:read") };
    let asked = 0;
    server.events.on("request:start", ({ request }) => { if (request.url.includes("/audit/")) asked++; });
    mount();
    await screen.findByRole("table", { name: "MCP tools" });
    expect(screen.queryByText("Recent calls")).toBeNull();
    expect(screen.queryByRole("table", { name: "Recent MCP calls" })).toBeNull();
    expect(asked).toBe(0);
    server.events.removeAllListeners();
  });

  it("keeps a loaded tools list when the status fails, without the operations sentence", async () => {
    server.use(http.get("/api/v1/mcp/status", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    const table = await screen.findByRole("table", { name: "MCP tools" });
    expect(within(table).getByText("services_list")).toBeInTheDocument();
    expect(screen.queryByText(/operations in all/)).toBeNull();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/status/i);
    expect(alert).not.toHaveTextContent(/load the tools/i);
    expect(screen.getByText(URL_())).toBeInTheDocument();
  });

  it("says when the tools cannot be loaded and still shows the snippets", async () => {
    server.use(http.get("/api/v1/mcp/tools", () => HttpResponse.json({ error: "boom" }, { status: 500 })));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent(/tools/i);
    expect(screen.queryByRole("table", { name: "MCP tools" })).toBeNull();
    expect(screen.getByText(/claude mcp add --transport http burrow/)).toBeInTheDocument();
  });
});
