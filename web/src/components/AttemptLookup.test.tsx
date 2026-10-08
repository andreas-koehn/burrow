import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { AttemptLookup } from "@/components/AttemptLookup";

// The fixtures hold the log of req-1: zai failed with a 500, openrouter answered.
function renderLookup() {
  return renderApp(<AttemptLookup />, "/gateway/models");
}

async function lookUp(id: string) {
  await userEvent.type(screen.getByLabelText("Request id"), id);
  await userEvent.click(screen.getByRole("button", { name: "Look up" }));
}

describe("AttemptLookup", () => {
  it("shows what a request tried", async () => {
    renderLookup();
    await lookUp("req-1");
    const rows = await screen.findAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(within(rows[0]!).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Order", "Target", "Result", "Duration"]);
    expect(within(rows[1]!).getByText("zai/glm-5.1")).toBeInTheDocument();
    expect(within(rows[1]!).getByText("HTTP 500")).toBeInTheDocument();
    expect(within(rows[1]!).getByText("812 ms")).toBeInTheDocument();
    expect(within(rows[2]!).getByText("openrouter/google/gemini-x")).toBeInTheDocument();
    expect(within(rows[2]!).getByText("answered")).toBeInTheDocument();
    expect(within(rows[2]!).getByText("1.4 s")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Attempts of request req-1" })).toBeInTheDocument();
  });

  it("explains an empty result", async () => {
    renderLookup();
    await lookUp("unknown");
    expect(await screen.findByText(/no attempt log for this request/i)).toBeInTheDocument();
    expect(screen.getByText(/only kept for requests that needed more than one attempt or failed/i)).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("names every result in words", async () => {
    db.aiAttempts["req-2"] = [
      { position: 0, provider: "zai", model: "a", status: 0, error_code: "timeout", duration_ms: 60000, ts: "2026-10-06T09:30:00Z" },
      { position: 1, provider: "zai", model: "b", status: 0, error_code: "no_response", duration_ms: 12, ts: "2026-10-06T09:31:00Z" },
      { position: 2, provider: "zai", model: "c", status: 429, error_code: "busy", duration_ms: 5, ts: "2026-10-06T09:31:01Z" },
      { position: 3, provider: "zai", model: "d", status: 0, error_code: "breaker_open", duration_ms: 0, ts: "2026-10-06T09:31:02Z" },
      { position: 4, provider: "zai", model: "e", status: 0, error_code: "dialect_mismatch", duration_ms: 0, ts: "2026-10-06T09:31:03Z" },
      { position: 5, provider: "zai", model: "f", status: 0, error_code: "client_closed", duration_ms: 3, ts: "2026-10-06T09:31:04Z" },
      { position: 6, provider: "zai", model: "g", status: 200, error_code: "stream_aborted", duration_ms: 900, ts: "2026-10-06T09:31:05Z" },
      { position: 7, provider: "zai", model: "h", status: 0, error_code: "panic", duration_ms: 1, ts: "2026-10-06T09:31:06Z" },
      { position: 8, provider: "zai", model: "i", status: 429, error_code: "http_429", duration_ms: 40, ts: "2026-10-06T09:31:07Z" },
      // Translated attempts.
      { position: 9, provider: "zai", model: "j", status: 200, error_code: "upstream_invalid", duration_ms: 40, ts: "2026-10-06T09:31:08Z" },
      { position: 10, provider: "zai", model: "k", status: 200, error_code: "upstream_error", duration_ms: 40, ts: "2026-10-06T09:31:09Z" },
      { position: 11, provider: "zai", model: "l", status: 0, error_code: "translate_error", duration_ms: 1, ts: "2026-10-06T09:31:10Z" },
    ];
    renderLookup();
    await lookUp("req-2");
    const rows = await screen.findAllByRole("row");
    const result = (i: number) => within(rows[i]!).getAllByRole("cell")[2]!.textContent;
    expect(rows.slice(1).map((_, i) => result(i + 1))).toEqual([
      "timed out", "no response", "no free place", "skipped: provider failing", "skipped: other API format",
      "client left", "stream broke off", "internal error", "HTTP 429",
      "answer could not be translated", "provider reported an error", "request could not be translated",
    ]);
    // Order counts from 1, in the order the attempts were made.
    expect(within(rows[1]!).getAllByRole("cell")[0]).toHaveTextContent("1");
    expect(within(rows[9]!).getAllByRole("cell")[0]).toHaveTextContent("9");
  });

  it("sends an id with a slash percent-encoded", async () => {
    let path = "";
    server.use(http.get("/api/v1/ai/requests/:id/attempts", ({ request }) => {
      path = new URL(request.url).pathname;
      return HttpResponse.json([]);
    }));
    renderLookup();
    await lookUp("  relay-1/AbC-000042 ");
    await screen.findByText(/no attempt log for this request/i);
    expect(path).toBe("/api/v1/ai/requests/relay-1%2FAbC-000042/attempts");
  });

  it("needs an id, and says when the relay refuses", async () => {
    server.use(http.get("/api/v1/ai/requests/:id/attempts", () => HttpResponse.json({ error: "admin required" }, { status: 403 })));
    renderLookup();
    expect(screen.getByRole("button", { name: "Look up" })).toBeDisabled();
    await lookUp("req-1");
    expect(await screen.findByRole("alert")).toHaveTextContent("Only an administrator can read attempt logs.");
  });

  it("shows the relay's reason for another failure and offers a retry", async () => {
    let calls = 0;
    server.use(http.get("/api/v1/ai/requests/:id/attempts", () => {
      calls++;
      return calls === 1 ? HttpResponse.json({ error: "internal error" }, { status: 500 }) : HttpResponse.json([]);
    }));
    renderLookup();
    await lookUp("req-1");
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load the attempts: internal error");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText(/no attempt log for this request/i)).toBeInTheDocument();
  });

  it("the mock refuses what the relay refuses", async () => {
    renderLookup();
    await lookUp("x".repeat(129));
    expect(await screen.findByRole("alert")).toHaveTextContent("request id must be 1-128 characters without control characters");
  });
});
