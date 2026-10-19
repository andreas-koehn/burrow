import { describe, it, expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { RedactionRuleDialog } from "@/components/RedactionRuleDialog";

describe("RedactionRuleDialog scopes", () => {
  it("offers the two tool scopes and sends the chosen one", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(http.post("/api/v1/redaction/rules", async ({ request }) => {
      posted = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({ id: "r1", name: "n", pattern: "p", action: "mask", scope: "tool_result" }, { status: 201 });
    }));
    renderApp(<RedactionRuleDialog open onClose={() => {}} />, "/gateway/guardrails");
    await userEvent.type(screen.getByLabelText("Name"), "secrets");
    await userEvent.type(screen.getByLabelText("Pattern"), "sk-abc");
    await userEvent.click(screen.getByRole("combobox", { name: "Scope" }));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["Request and response", "Request body", "Response body", "Tool arguments", "Tool results"]);
    await userEvent.click(screen.getByRole("option", { name: "Tool results" }));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(posted).toMatchObject({ name: "secrets", scope: "tool_result" }));
  });
});
