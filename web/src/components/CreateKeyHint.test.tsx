import { describe, it, expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { http, HttpResponse, delay } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { CreateKeyHint } from "@/components/CreateKeyHint";
import type { QueryClient } from "@tanstack/react-query";
import type { AiGatewayKey } from "@/lib/contract";

const key = (over: Partial<AiGatewayKey>): AiGatewayKey => ({ ...db.aiGatewayKeys[0]!, ...over });

/** Both answers the hint reads are in, so "nothing shown" is not just "not yet". */
async function settled(qc: QueryClient) {
  await waitFor(() => {
    expect(qc.getQueryState(["ai", "keys"])?.status).toBe("success");
    expect(qc.getQueryState(["me"])?.status).toBe("success");
  });
}

describe("CreateKeyHint", () => {
  it("shows nothing when a key exists", async () => {
    const { qc, container } = renderApp(<CreateKeyHint provider="ollama" />);
    await settled(qc);
    expect(container).toBeEmptyDOMElement();
  });

  it("another user's key is not the reader's: an admin who owns none is offered one", async () => {
    // An admin is sent every user's keys.
    db.aiGatewayKeys = [key({ id: "gk_bob", name: "bobs-ci", user_id: "bur_usr_bob0002" })];
    renderApp(<CreateKeyHint provider="ollama" />);
    expect(await screen.findByText("You have no gateway key yet.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Create a gateway key" })).toHaveAttribute("href", "/gateway/keys?new=1&provider=ollama");
  });

  it.each([
    ["everything from another provider", ["llamacpp/*"]],
    ["a model name", ["burrow-simple"]],
    ["one model of this provider", ["ollama/qwen2.5:0.5b"]],
  ])("a key limited to %s does not open the provider: says so, with the same link", async (_name, allowed_models) => {
    db.aiGatewayKeys = [key({ allowed_models })];
    renderApp(<CreateKeyHint provider="ollama" />);
    expect(await screen.findByText("None of your gateway keys opens this provider.")).toBeInTheDocument();
    expect(screen.queryByText("You have no gateway key yet.")).toBeNull();
    expect(screen.getByRole("link", { name: "Create a gateway key" })).toHaveAttribute("href", "/gateway/keys?new=1&provider=ollama");
  });

  it("a key limited to everything from this provider opens it: nothing shown", async () => {
    db.aiGatewayKeys = [key({ allowed_models: ["burrow-simple", "ollama/*"] })];
    const { qc, container } = renderApp(<CreateKeyHint provider="ollama" />);
    await settled(qc);
    expect(container).toBeEmptyDOMElement();
  });

  it("without a provider any key of the reader's counts, limited or not", async () => {
    db.aiGatewayKeys = [key({ allowed_models: ["burrow-simple"] })];
    const { qc, container } = renderApp(<CreateKeyHint />);
    await settled(qc);
    expect(container).toBeEmptyDOMElement();
  });

  it("offers to create one when there is none", async () => {
    db.aiGatewayKeys = [];
    renderApp(<CreateKeyHint provider="ollama" />);
    expect(await screen.findByText("You have no gateway key yet.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Create a gateway key" })).toHaveAttribute("href", "/gateway/keys?new=1&provider=ollama");
  });

  it("without a provider the link opens the plain dialog", async () => {
    db.aiGatewayKeys = [];
    renderApp(<CreateKeyHint />);
    expect(await screen.findByRole("link", { name: "Create a gateway key" })).toHaveAttribute("href", "/gateway/keys?new=1");
  });

  it("a revoked key is no key", async () => {
    db.aiGatewayKeys = db.aiGatewayKeys.map((k) => ({ ...k, revoked_at: "2026-05-19T08:00:00Z" }));
    renderApp(<CreateKeyHint />);
    expect(await screen.findByText("You have no gateway key yet.")).toBeInTheDocument();
  });

  it("shows nothing while loading or when the list cannot be read", async () => {
    // Loading: no flash of the hint before the answer says there is a key.
    let release = () => {};
    const held = new Promise<void>((r) => { release = r; });
    server.use(http.get("/api/v1/ai/keys", async () => {
      await held;
      return HttpResponse.json([]);
    }));
    const first = renderApp(<CreateKeyHint provider="ollama" />);
    await delay(50);
    expect(first.container).toBeEmptyDOMElement();
    release();
    expect(await screen.findByText("You have no gateway key yet.")).toBeInTheDocument();
    first.unmount();

    // No error for someone who may not list keys.
    server.use(http.get("/api/v1/ai/keys", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    const second = renderApp(<CreateKeyHint provider="ollama" />);
    await waitFor(() => expect(second.qc.getQueryState(["ai", "keys"])?.status).toBe("error"));
    expect(second.container).toBeEmptyDOMElement();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
