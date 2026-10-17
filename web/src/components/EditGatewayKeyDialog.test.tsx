import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
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
