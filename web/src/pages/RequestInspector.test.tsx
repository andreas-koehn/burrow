import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import RequestInspector from "@/pages/RequestInspector";

function mount(path = "/gateway/requests/svc_ai001") {
  return renderApp(
    <Routes>
      <Route path="/gateway/requests/:serviceId/:requestId?" element={<RequestInspector />} />
    </Routes>,
    path,
  );
}

describe("Request inspector (§4.23)", () => {
  it("renders a two-pane layout with the request list on the left", async () => {
    mount();
    expect(await screen.findByRole("table", { name: /requests/i })).toBeInTheDocument();
    // The right pane prompts the user to pick a request when none selected.
    expect(screen.getByText(/select a request/i)).toBeInTheDocument();
  });

  it("clicking a row populates the right pane with the redacted headers table", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const rows = await within(table).findAllByRole("row");
    // skip the header row
    await userEvent.click(rows[1]!);
    expect(await screen.findByRole("tab", { name: /^request$/i })).toBeInTheDocument();
    // The headers table appears with at least one redacted value cell.
    const headersTable = await screen.findByRole("table", { name: /^headers$/i });
    expect(within(headersTable).getByText(/\[redacted\]/)).toBeInTheDocument();
  });

  it("a translated request names the direction and lists what was left out", async () => {
    const e = db.inspectorEntries.svc_ai001![0]!;
    e.translated = "messages-chat";
    e.dropped = ["cache_control", "top_k"];
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const note = await screen.findByRole("group", { name: "Translation" });
    expect(within(note).getByText("Translated")).toBeInTheDocument();
    expect(within(note).getByText("Anthropic Messages → Chat Completions")).toBeInTheDocument();
    const list = within(note).getByRole("list", { name: "Left out" });
    expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["cache_control", "top_k"]);
  });

  it("a final 'more' is not a field: the list says it is incomplete", async () => {
    const e = db.inspectorEntries.svc_ai001![0]!;
    e.translated = "responses-messages";
    e.dropped = ["store", "more"];
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const note = await screen.findByRole("group", { name: "Translation" });
    expect(within(note).getByText("OpenAI Responses → Anthropic Messages")).toBeInTheDocument();
    const list = within(note).getByRole("list", { name: "Left out" });
    expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["store"]);
    expect(within(note).getByText("and more")).toBeInTheDocument();
  });

  it("a dropped name is shown as text, never as markup or a link", async () => {
    const e = db.inspectorEntries.svc_ai001![0]!;
    e.translated = "messages-chat";
    e.dropped = ["unknown:<img src=x onerror=alert(1)>", "unknown:https://evil.example/x"];
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const note = await screen.findByRole("group", { name: "Translation" });
    expect(within(note).getByText("unknown:<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(within(note).getByText("unknown:https://evil.example/x")).toBeInTheDocument();
    expect(note.querySelector("img")).toBeNull();
    expect(note.querySelector("a")).toBeNull();
  });

  it("a translated request that left nothing out shows no list", async () => {
    const e = db.inspectorEntries.svc_ai001![0]!;
    e.translated = "chat-messages";
    e.dropped = [];
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const note = await screen.findByRole("group", { name: "Translation" });
    expect(within(note).getByText("Chat Completions → Anthropic Messages")).toBeInTheDocument();
    expect(within(note).queryByRole("list")).toBeNull();
    expect(within(note).queryByText(/left out/i)).toBeNull();
  });

  it("a request served in its own format shows no translation note", async () => {
    const e = db.inspectorEntries.svc_ai001![0]!;
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    expect(await screen.findByRole("tab", { name: /^request$/i })).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Translation" })).toBeNull();
  });

  it("the list marks a translated request", async () => {
    db.inspectorEntries.svc_ai001![0]!.translated = "messages-chat";
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    expect(within(table).getAllByText("translated", { selector: "span.badge" })).toHaveLength(1);
  });

  it("says the inspector is off and how to turn it on when inspector.enabled is false", async () => {
    db.aiConfigs.svc_ai001.inspector.enabled = false;
    mount();
    expect(await screen.findByText("Request inspector is off for this service.")).toBeInTheDocument();
    const hint = screen.getByText(/The dashboard has no switch for this yet/);
    expect(hint).toHaveTextContent("inspector.enabled");
    expect(hint).toHaveTextContent("PUT /api/v1/services/svc_ai001/ai-config");
    expect(screen.queryByText(/Access settings/)).toBeNull();
  });

  it("Replay POSTs /services/:id/inspector/requests/:rid/replay", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const rows = await within(table).findAllByRole("row");
    await userEvent.click(rows[1]!);
    // Open the replay dialog from the detail toolbar, then confirm.
    await userEvent.click(await screen.findByRole("button", { name: /open replay dialog/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^replay$/i }));
    await waitFor(() => {
      expect(
        fetchSpy.mock.calls.some(([url, init]) =>
          /\/api\/v1\/services\/svc_ai001\/inspector\/requests\/[^/]+\/replay$/.test(String(url))
          && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true);
    });
  });

  it("a request is a link in its row, says how long ago it came in, and opens from the keyboard", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const links = await within(table).findAllByRole("link");
    expect(links[0]).toHaveAttribute("href", expect.stringMatching(/^\/gateway\/requests\/svc_ai001\/.+/));
    const row = links[0]!.closest("tr")!;
    // A plain row: aria-selected belongs to grid rows, which this table has none of.
    expect(row).not.toHaveAttribute("aria-selected");
    const when = row.querySelector("time")!;
    expect(when).toHaveAttribute("datetime");
    expect(when.getAttribute("title")).toMatch(/\d{2}:\d{2}/);
    links[0]!.focus();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByRole("tab", { name: /^request$/i })).toBeInTheDocument();
    // The open one is marked on its link.
    expect(within(table).getAllByRole("link")[0]).toHaveAttribute("aria-current", "true");
  });

  it("a request opened in a new tab with a key held leaves this tab's request as it is", async () => {
    const first = db.inspectorEntries.svc_ai001![0]!.id;
    mount(`/gateway/requests/svc_ai001/${first}`);
    const table = await screen.findByRole("table", { name: /requests/i });
    const links = await within(table).findAllByRole("link");
    await waitFor(() => expect(links[0]).toHaveAttribute("aria-current", "true"));
    const user = userEvent.setup();
    await user.keyboard("{Control>}");
    await user.click(links[1]!);
    await user.keyboard("{/Control}");
    // Still the request this tab shows: Replay here must not send the other one.
    expect(links[0]).toHaveAttribute("aria-current", "true");
    expect(links[1]).not.toHaveAttribute("aria-current");
  });

  it("inspector rows are reachable via table[aria-label=Requests] tbody tr.clickable (capture-harness guard)", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const clickable = within(table).getAllByRole("row").filter((r) => r.classList.contains("clickable"));
    expect(clickable.length).toBeGreaterThan(0);
  });

  it("lets the user switch to another http service (F7)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "svc-a", name: "alpha", type: "http", access_mode: "api_key", connected: true },
      { id: "svc-b", name: "beta", type: "http", access_mode: "api_key", connected: false },
    ])));
    mount();
    const picker = await screen.findByRole("combobox", { name: /service/i });
    expect(picker).toHaveAttribute("id", "inspector-service");
    await userEvent.click(picker);
    await userEvent.click(await screen.findByRole("option", { name: "beta" }));
    // Picking a service navigates to its inspector: the trigger now shows it.
    await waitFor(() => {
      expect(document.getElementById("inspector-service")).toHaveTextContent("beta");
    });
  });

  it("shows no service picker when there is only one http service (F7)", async () => {
    server.use(http.get("/api/v1/services", () => HttpResponse.json([
      { id: "svc_ai001", name: "ollama", type: "http", access_mode: "api_key", connected: true },
      { id: "tcp-1", name: "ssh", type: "tcp", access_mode: "open", connected: true },
    ])));
    mount();
    // The subtitle names the service only once /services has resolved.
    await screen.findByText("Tail and replay traffic on ollama.");
    expect(document.getElementById("inspector-service")).toBeNull();
  });

  it("shows the API error with a way back for an unknown service, not an endless skeleton", async () => {
    mount("/gateway/requests/no-such-service");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load requests: service not found");
    expect(within(alert).getByRole("link", { name: /request inspector/i })).toHaveAttribute("href", "/gateway/requests");
    expect(screen.queryByRole("table", { name: /requests/i })).not.toBeInTheDocument();
  });

  it("clears the search filter when switching service (F7)", async () => {
    db.services.push({ ...db.services.find((s) => s.id === "svc_ai001")!, id: "svc_other", name: "other" });
    db.inspectorEntries.svc_other = db.inspectorEntries.svc_ai001!;
    // Captured requests mean capture is on for it: a service without a stored config captures nothing.
    db.aiConfigs.svc_other = db.aiConfigs.svc_ai001!;
    mount();
    const search = await screen.findByRole("searchbox", { name: /search requests/i });
    await userEvent.type(search, "zzz-no-match");
    expect(await screen.findByText("No requests match your search.")).toBeInTheDocument();
    expect(screen.queryByText("No requests yet.")).not.toBeInTheDocument();
    expect(screen.queryByText(/Requests appear here as soon as traffic/)).not.toBeInTheDocument();
    await userEvent.click(document.getElementById("inspector-service")!);
    await userEvent.click(await screen.findByRole("option", { name: "other" }));
    await waitFor(() => {
      expect(screen.getByRole("searchbox", { name: /search requests/i })).toHaveValue("");
    });
    expect(screen.queryByText("No requests yet.")).not.toBeInTheDocument();
    expect(screen.queryByText("No requests match your search.")).not.toBeInTheDocument();
  });

  it("keeps the loaded list when a background refetch fails", async () => {
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    const rows = await within(table).findAllByRole("row");
    await userEvent.click(rows[1]!);
    // From here on the list endpoint fails; a replay invalidates and refetches it.
    let failed = 0;
    server.use(http.get("/api/v1/services/:id/inspector/requests", () => {
      failed += 1;
      return HttpResponse.json({ error: "boom" }, { status: 500 });
    }));
    await userEvent.click(await screen.findByRole("button", { name: /open replay dialog/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^replay$/i }));
    await waitFor(() => expect(failed).toBeGreaterThan(0));
    // Let the failed refetch settle into the query state before asserting.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByRole("table", { name: /requests/i })).toBeInTheDocument();
    expect(screen.queryByText(/couldn't load requests/i)).not.toBeInTheDocument();
  });

  it("the unknown-service error offers a Retry that refetches the list", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/services/:id/inspector/requests", () => {
      calls += 1;
      if (calls === 1) return HttpResponse.json({ error: "boom" }, { status: 500 });
      return HttpResponse.json([]);
    }));
    mount();
    const alert = await screen.findByRole("alert");
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: /requests/i })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  describe("guardrail outcome and binary bodies", () => {
    const first = () => db.inspectorEntries.svc_ai001![0]!;

    it("the detail header names the pattern, the direction and the action", async () => {
      const e = first();
      e.guardrail = { pattern: "aws-key", direction: "answer", action: "block", mid_stream: true };
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText("Blocked mid-answer · aws-key")).toBeInTheDocument();
      expect(within(note).getByText("Answer")).toBeInTheDocument();
    });

    it("shows the judge's reason in full", async () => {
      const e = first();
      const reason = "The request asks for step-by-step malware development. ".repeat(4).trim();
      e.judge = { direction: "request", verdict: "block", reason };
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText("Judged · refused")).toBeInTheDocument();
      expect(within(note).getByText(reason)).toBeInTheDocument();
    });

    it("names a judge failure by its class", async () => {
      const e = first();
      e.judge = { direction: "request", verdict: "failed", failure: "malformed" };
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText("Judge failed · malformed")).toBeInTheDocument();
    });

    it("a judge failure that refused shows the class and the outcome", async () => {
      const e = first();
      e.judge = { direction: "request", verdict: "failed", failure: "unavailable", refused: true };
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText("Judge failed · refused")).toBeInTheDocument();
      expect(within(note).getByText(/unavailable/)).toBeInTheDocument();
    });

    it("a judge failure that let the request through says so", async () => {
      const e = first();
      e.judge = { direction: "request", verdict: "failed", failure: "timeout" };
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText(/let through/i)).toBeInTheDocument();
    });

    it("says when the pattern matched the gateway key's system text", async () => {
      const e = first();
      e.guardrail = { pattern: "ignore-previous", direction: "request", action: "refuse_403", in: "key_system_text" };
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText("Blocked · ignore-previous (key system text)")).toBeInTheDocument();
      expect(within(note).getByText("Matched the gateway key's system text.")).toBeInTheDocument();
    });

    it("marks the relay's own call to the judge", async () => {
      const e = first();
      e.purpose = "guardrail-judge";
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const note = await screen.findByRole("group", { name: "Guardrail" });
      expect(within(note).getByText("Judge call")).toBeInTheDocument();
    });

    it("has no guardrail group for a plain request", async () => {
      mount(`/gateway/requests/svc_ai001/${first().id}`);
      await screen.findByRole("tab", { name: /^request$/i });
      expect(screen.queryByRole("group", { name: "Guardrail" })).toBeNull();
    });

    it("a binary request and response body say so instead of showing the marker", async () => {
      const e = first();
      e.path = "/v1/audio/transcriptions";
      e.req_body = "<binary, 2048 bytes>";
      e.resp_body = "<binary, 15 bytes>";
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      expect(await screen.findByText("Binary body, 2048 bytes, not stored.")).toBeInTheDocument();
      expect(screen.queryByText("<binary, 2048 bytes>")).toBeNull();
      await userEvent.click(screen.getByRole("tab", { name: /^response$/i }));
      expect(await screen.findByText("Binary body, 15 bytes, not stored.")).toBeInTheDocument();
    });

    it("a text body can be reached and scrolled by keyboard, and is named", async () => {
      const e = first();
      e.req_body = '{"model":"m"}';
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      const pre = (await screen.findByText('{"model":"m"}')).closest("pre");
      expect(pre).toHaveAttribute("tabindex", "0");
      expect(pre).toHaveAccessibleName("Request body");
    });

    it("a body that only looks like the marker inside other text is shown as it is", async () => {
      const e = first();
      e.req_body = '{"text":"<binary, 5 bytes>"}';
      mount(`/gateway/requests/svc_ai001/${e.id}`);
      expect(await screen.findByText('{"text":"<binary, 5 bytes>"}')).toBeInTheDocument();
    });
  });
});

describe("Request inspector: MCP tool calls", () => {
  function toolEntry() {
    const e = db.inspectorEntries.svc_ai001![0]!;
    e.path = "/mcp/github";
    e.req_body = '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"list_issues","arguments":{"repo":"[REDACTED]"}}}';
    e.resp_body = '{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"3 open issues"}],"isError":false}}';
    e.mcp = { method: "tools/call", tool: "list_issues", params: { name: "list_issues", arguments: { repo: "[REDACTED]" } } };
    e.provider = "github";
    return e;
  }

  it("shows method, tool, arguments and result", async () => {
    const e = toolEntry();
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const panel = await screen.findByRole("group", { name: "MCP call" });
    expect(within(panel).getByText("tools/call")).toBeInTheDocument();
    expect(within(panel).getByText("list_issues")).toBeInTheDocument();
    expect(within(panel).getByText("github")).toBeInTheDocument();
    expect(within(panel).getByRole("region", { name: "Arguments" })).toHaveTextContent('"repo": "[REDACTED]"');
    expect(within(panel).getByRole("region", { name: "Result" })).toHaveTextContent("3 open issues");
  });

  it("says when the result is an error", async () => {
    const e = toolEntry();
    e.resp_body = '{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"boom"}],"isError":true}}';
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const panel = await screen.findByRole("group", { name: "MCP call" });
    expect(within(panel).getByText("The tool reported an error.")).toBeInTheDocument();
  });

  it("a tool call cannot be replayed: its side effects would run again", async () => {
    const e = toolEntry();
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    await screen.findByRole("group", { name: "MCP call" });
    expect(screen.queryByRole("button", { name: "Open replay dialog" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Replay/ })).toBeNull();
  });

  it("an ordinary request can be replayed", async () => {
    const e = db.inspectorEntries.svc_ai001![1]!;
    e.mcp = undefined;
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    expect(await screen.findByRole("button", { name: "Open replay dialog" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Replay & compare/ })).toBeInTheDocument();
  });

  it("reads the result from an SSE data: body", async () => {
    const e = toolEntry();
    e.resp_body = 'event: message\ndata: {"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"via sse"}],"isError":false}}\n\n';
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    const panel = await screen.findByRole("group", { name: "MCP call" });
    expect(within(panel).getByRole("region", { name: "Result" })).toHaveTextContent("via sse");
  });

  it("an ordinary request has no MCP panel", async () => {
    const e = db.inspectorEntries.svc_ai001![1]!;
    e.mcp = undefined;
    mount(`/gateway/requests/svc_ai001/${e.id}`);
    await screen.findByRole("tab", { name: /^request$/i });
    expect(screen.queryByRole("group", { name: "MCP call" })).toBeNull();
  });

  it("lists a tool call by its tool name in the path column", async () => {
    toolEntry();
    mount();
    const table = await screen.findByRole("table", { name: /requests/i });
    expect(await within(table).findByText("tools/call · list_issues")).toBeInTheDocument();
  });
});
