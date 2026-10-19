import { describe, it, expect, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { EditGatewayKeyDialog } from "@/components/EditGatewayKeyDialog";

function renderDialog() {
  const onOpenChange = vi.fn();
  renderApp(<EditGatewayKeyDialog open onOpenChange={onOpenChange} gatewayKey={db.aiGatewayKeys[0]!} />, "/gateway/keys");
  return { onOpenChange };
}

describe("EditGatewayKeyDialog", () => {
  it("starts with the key's name and text; nothing to save until something changes", () => {
    db.aiGatewayKeys[0]!.system_text = "Hi.";
    renderDialog();
    expect(screen.getByLabelText("Name")).toHaveValue("laptop");
    expect(screen.getByLabelText("System text")).toHaveValue("Hi.");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("the byte counter is announced politely while typing", async () => {
    db.aiGatewayKeys[0]!.system_text = "";
    renderDialog();
    await userEvent.type(screen.getByLabelText("System text"), "Hi");
    expect(screen.getByText("2 / 8192 bytes")).toHaveAttribute("aria-live", "polite");
  });

  it("an empty name cannot be saved", async () => {
    renderDialog();
    await userEvent.clear(screen.getByLabelText("Name"));
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(document.getElementById("edit-key-hint")).toHaveTextContent("Enter a name.");
  });

  it("sends the changed text only and closes", async () => {
    let put: Record<string, unknown> | null = null;
    server.use(http.put("/api/v1/ai/keys/gk_laptop1", async ({ request }) => {
      put = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({});
    }));
    const { onOpenChange } = renderDialog();
    await userEvent.type(screen.getByLabelText("System text"), "Be brief.");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(put).toEqual({ system_text: "Be brief." });
  });

  it("shows the relay's reason for another failure", async () => {
    server.use(http.put("/api/v1/ai/keys/gk_laptop1", () => HttpResponse.json({ error: "system_text is longer than 8192 bytes" }, { status: 400 })));
    renderDialog();
    await userEvent.type(screen.getByLabelText("System text"), "x");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("system_text is longer than 8192 bytes");
  });
});

describe("EditGatewayKeyDialog: allow-lists", () => {
  function capturePut() {
    const box: { body: Record<string, unknown> | null } = { body: null };
    server.use(http.put("/api/v1/ai/keys/gk_laptop1", async ({ request }) => {
      box.body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json({});
    }));
    return box;
  }

  it("shows the key's tools and read-only setting", () => {
    db.aiGatewayKeys[0]!.allowed_tools = ["github__*"];
    db.aiGatewayKeys[0]!.tools_read_only = true;
    renderDialog();
    expect(within(screen.getByRole("list", { name: "Allowed tools" })).getByText("github__*")).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Read-only tools only" })).toHaveAttribute("aria-checked", "true");
  });

  it("sends only the tool list when only it changed", async () => {
    db.aiGatewayKeys[0]!.allowed_tools = [];
    const put = capturePut();
    const { onOpenChange } = renderDialog();
    await userEvent.type(screen.getByLabelText("Tool pattern"), "files__read_file{Enter}");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(put.body).toEqual({ allowed_tools: ["files__read_file"] });
  });

  it("removing the last tool sends an empty list, which means no tools", async () => {
    db.aiGatewayKeys[0]!.allowed_tools = ["github__*"];
    const put = capturePut();
    renderDialog();
    await userEvent.click(screen.getByRole("button", { name: "Remove github__*" }));
    expect(screen.getByText(/No tools selected/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put.body).toEqual({ allowed_tools: [] }));
  });

  it("sends the read-only flag alone when only it changed", async () => {
    db.aiGatewayKeys[0]!.tools_read_only = false;
    const put = capturePut();
    renderDialog();
    await userEvent.click(screen.getByRole("switch", { name: "Read-only tools only" }));
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put.body).toEqual({ tools_read_only: true }));
  });

  it("edits the model list: only these models, then back to all", async () => {
    db.aiGatewayKeys[0]!.allowed_models = [];
    const put = capturePut();
    renderDialog();
    await userEvent.click(screen.getByRole("radio", { name: "Only these models" }));
    // Nothing chosen would mean every model.
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    await userEvent.click(await screen.findByRole("checkbox", { name: "burrow-simple" }));
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put.body).toEqual({ allowed_models: ["burrow-simple"] }));
  });

  it("clearing the model restriction sends an empty list", async () => {
    db.aiGatewayKeys[0]!.allowed_models = ["burrow-simple"];
    const put = capturePut();
    renderDialog();
    expect(await screen.findByRole("checkbox", { name: "burrow-simple" })).toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: "All models" }));
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put.body).toEqual({ allowed_models: [] }));
  });
});
