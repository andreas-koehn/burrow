import { describe, it, expect } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { Route, Routes, useLocation } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import ToolSources from "@/pages/ToolSources";
import { ALL_PERMISSIONS } from "@/auth/permissions";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

function mount() {
  return renderApp(
    <>
      <Routes>
        <Route path="/gateway/tools" element={<ToolSources />} />
        <Route path="*" element={null} />
      </Routes>
      <Where />
    </>,
    "/gateway/tools",
  );
}

function recordPosts() {
  const made: { path: string; body: Record<string, unknown> | null }[] = [];
  server.events.on("request:start", async ({ request }) => {
    if (request.method !== "POST") return;
    made.push({
      path: new URL(request.url).pathname.replace("/api/v1", ""),
      body: (await request.clone().json().catch(() => null)) as Record<string, unknown> | null,
    });
  });
  return made;
}

function asReader() {
  db.me = { ...db.me, role: "user", permissions: ["mcp:sources:read"] };
}

describe("Tools page", () => {
  it("lists the sources with kind, status and tools", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Tool sources" });
    const gh = within(table).getByRole("row", { name: /GitHub/ });
    expect(within(gh).getByRole("link", { name: "GitHub" })).toHaveAttribute("href", "/gateway/tools/github");
    expect(within(gh).getByText("Remote")).toBeInTheDocument();
    expect(within(gh).getByText("https://mcp.example.com/mcp")).toBeInTheDocument();
    expect(within(gh).getByText("2 to approve")).toBeInTheDocument();
    expect(within(gh).getByText("3")).toBeInTheDocument();
    const files = within(table).getByRole("row", { name: /Files/ });
    expect(within(files).getByText("On a machine")).toBeInTheDocument();
    expect(within(files).getByText("Not synced yet")).toBeInTheDocument();
  });

  it("says what a source is for when there is none, and offers both ways to add one", async () => {
    db.toolSources = [];
    mount();
    expect(await screen.findByText("No tool sources yet")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Add remote source" }).length).toBeGreaterThan(0);
    expect(screen.getAllByRole("button", { name: "Add local source" }).length).toBeGreaterThan(0);
  });

  it("a reader sees the list and no way to add", async () => {
    asReader();
    mount();
    await screen.findByRole("table", { name: "Tool sources" });
    expect(screen.queryByRole("button", { name: "Add remote source" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add local source" })).not.toBeInTheDocument();
  });
});

describe("Add remote source", () => {
  async function open() {
    mount();
    await screen.findByRole("table", { name: "Tool sources" });
    await userEvent.click(screen.getAllByRole("button", { name: "Add remote source" })[0]!);
    return screen.findByRole("dialog");
  }

  it("sends what was typed and opens the new source", async () => {
    const posts = recordPosts();
    const dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Linear Tools");
    expect(within(dialog).getByLabelText("Slug")).toHaveValue("linear-tools");
    expect(within(dialog).getByText(/\/mcp\/linear-tools/)).toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("URL"), "https://mcp.linear.example/mcp");
    await userEvent.type(within(dialog).getByLabelText("Credential slot"), "LINEAR");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add source" }));
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("/gateway/tools/linear-tools"));
    const post = posts.find((p) => p.path === "/mcp/sources")!;
    expect(post.body).toEqual({
      slug: "linear-tools", name: "Linear Tools", kind: "remote", url: "https://mcp.linear.example/mcp", credential_slot: "LINEAR",
    });
  });

  it("sends the options of the advanced part only when they were set", async () => {
    const posts = recordPosts();
    const dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Docs");
    await userEvent.type(within(dialog).getByLabelText("URL"), "https://docs.example/mcp");
    await userEvent.click(within(dialog).getByRole("button", { name: "Advanced" }));
    await userEvent.type(within(dialog).getByLabelText("Header"), "X-Api-Key");
    await userEvent.click(within(dialog).getByLabelText("Format"));
    await userEvent.paste("{key}");
    await userEvent.type(within(dialog).getByLabelText("Calls per minute"), "30");
    await userEvent.type(within(dialog).getByLabelText("Revision"), "2025-06-18");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add source" }));
    await waitFor(() => expect(posts.some((p) => p.path === "/mcp/sources")).toBe(true));
    expect(posts.find((p) => p.path === "/mcp/sources")!.body).toEqual({
      slug: "docs", name: "Docs", kind: "remote", url: "https://docs.example/mcp",
      auth_header: "X-Api-Key", auth_format: "{key}", rpm: 30, protocol_version: "2025-06-18",
    });
  });

  it("does not send an address that is not https", async () => {
    const posts = recordPosts();
    const dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Docs");
    await userEvent.type(within(dialog).getByLabelText("URL"), "http://docs.example/mcp");
    await userEvent.tab();
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/https/i);
    expect(within(dialog).getByRole("button", { name: "Add source" })).toBeDisabled();
    expect(posts).toEqual([]);
  });

  it("refuses a name that makes a reserved slug", async () => {
    const dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "burrow");
    await userEvent.tab();
    expect(await within(dialog).findByText(/reserved/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Add source" })).toBeDisabled();
  });

  it("shows the relay's answer when the slug is taken", async () => {
    const dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "github");
    await userEvent.type(within(dialog).getByLabelText("URL"), "https://mcp.example.com/mcp");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add source" }));
    expect(await within(dialog).findByText(/already exists/)).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/gateway/tools");
    expect(screen.getByTestId("where")).not.toHaveTextContent("/gateway/tools/github");
  });
});

describe("Add local source", () => {
  // The second step is a dialog of its own: look it up again after Continue.
  async function next() {
    await screen.findByText("Run this on the machine", {}, { timeout: 4000 });
    return screen.getByRole("dialog");
  }

  async function open() {
    mount();
    await screen.findByRole("table", { name: "Tool sources" });
    await userEvent.click(screen.getAllByRole("button", { name: "Add local source" })[0]!);
    return screen.findByRole("dialog");
  }

  it("creates a service and a tunnel source, then shows the command and the burrow.yaml service", async () => {
    const posts = recordPosts();
    let dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Notes");
    await userEvent.type(within(dialog).getByLabelText("Command or address"), "npx -y @acme/notes-mcp /srv/notes");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    dialog = await next();
    expect(posts.map((p) => p.path)).toEqual(["/services", "/mcp/sources"]);
    expect(posts[0]!.body).toMatchObject({ service_id: "notes", title: "notes", access_mode: "api_key" });
    expect(posts[1]!.body).toMatchObject({ slug: "notes", name: "Notes", kind: "tunnel", service_id: "notes" });
    expect(within(dialog).getByRole("group", { name: "Run command" })).toHaveTextContent("burrow mcp notes -- npx -y @acme/notes-mcp /srv/notes");
    const yaml = within(dialog).getByRole("group", { name: /burrow.yaml service/i });
    expect(yaml).toHaveTextContent("type: mcp");
    expect(yaml).toHaveTextContent('command: "npx"');
    expect(yaml).toHaveTextContent('args: ["-y", "@acme/notes-mcp", "/srv/notes"]');
    expect(within(dialog).getByText("Waiting for the client…")).toBeInTheDocument();
  });

  it("shows the address form of the command for a server that listens already", async () => {
    let dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Notes");
    await userEvent.type(within(dialog).getByLabelText("Command or address"), "http://localhost:3001/mcp");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    dialog = await next();
    expect(await within(dialog).findByRole("group", { name: "Run command" }, { timeout: 4000 })).toHaveTextContent("burrow mcp notes http://localhost:3001/mcp");
    expect(within(dialog).getByRole("group", { name: /burrow.yaml service/i })).toHaveTextContent('url: "http://localhost:3001/mcp"');
  });

  it("once the client is connected it reads the tools and says what waits for approval", async () => {
    db.toolUpstream.notes = [
      { name: "read_note", description: "Reads a note.", inputSchema: { type: "object" } },
      { name: "write_note", description: "Writes a note.", inputSchema: { type: "object" } },
    ];
    let dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Notes");
    await userEvent.type(within(dialog).getByLabelText("Command or address"), "node notes.js");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    dialog = await next();
    await within(dialog).findByText("Waiting for the client…", {}, { timeout: 4000 });
    // The client comes online.
    db.services.find((s) => s.id === "notes")!.connected = true;
    expect(await within(dialog).findByText("2 tools found, 2 to approve", {}, { timeout: 6000 })).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Review the tools" }));
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("/gateway/tools/notes"));
  }, 15000);

  it("says so when the first read fails and reads again on request", async () => {
    let answers = 0;
    server.use(http.post("/api/v1/mcp/sources/notes/sync", () => {
      answers++;
      return answers === 1
        ? HttpResponse.json({ error: "the tool server did not answer" }, { status: 502 })
        : HttpResponse.json({ added: 1, changed: 0, removed: 0, tool_count: 1, pending_count: 1, skipped: [] });
    }));
    let dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Notes");
    await userEvent.type(within(dialog).getByLabelText("Command or address"), "node notes.js");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    dialog = await next();
    await within(dialog).findByText("Waiting for the client…", {}, { timeout: 4000 });
    db.services.find((s) => s.id === "notes")!.connected = true;
    expect(await within(dialog).findByText(/did not answer/, {}, { timeout: 6000 })).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Read the tools again" }));
    expect(await within(dialog).findByText("1 tool found, 1 to approve")).toBeInTheDocument();
  }, 15000);

  it("keeps what was made when the source cannot be created", async () => {
    const dialog = await open();
    await userEvent.type(within(dialog).getByLabelText("Name"), "github");
    await userEvent.type(within(dialog).getByLabelText("Command or address"), "node x.js");
    await userEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
    expect(await within(dialog).findByText(/already exists/)).toBeInTheDocument();
    expect(within(dialog).queryByText("Run this on the machine")).not.toBeInTheDocument();
  });
});

describe("permissions", () => {
  it("an admin holds everything", () => {
    expect(ALL_PERMISSIONS).toContain("mcp:sources:manage");
  });
});
