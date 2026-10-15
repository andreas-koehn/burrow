import { describe, it, expect, vi } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { delay, http, HttpResponse } from "msw";
import { server } from "@/mocks/server";
import { renderApp } from "@/mocks/test-utils";
import { db } from "@/mocks/db";
import { ApiKeysPanel } from "@/components/ApiKeysPanel";

const SVC = "svc_ai001"; // seeded http service in api_key mode with 2 keys

describe("ApiKeysPanel", () => {
  it("lists existing keys with created/last-used and a Revoke action", async () => {
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    const table = await screen.findByRole("table", { name: /api keys/i });
    const ci = (await within(table).findByText("ci")).closest("tr")!;
    // 'ci' has last_used null → "Never"
    expect(within(ci).getByText("Never")).toBeInTheDocument();
    expect(within(ci).getByRole("button", { name: /revoke key ci/i })).toBeInTheDocument();
    const prod = within(table).getByText("prod").closest("tr")!;
    // Created and Last used: how long ago (the date, past 30 days), the exact UTC time on hover.
    const [created, lastUsed] = Array.from(prod.querySelectorAll("time"));
    expect(created).toHaveTextContent("01 May 2026");
    expect(created).toHaveAttribute("title", "01 May 2026, 08:00:00 UTC");
    expect(lastUsed).toHaveTextContent("18 May 2026");
    expect(lastUsed).toHaveAttribute("title", "18 May 2026, 09:00:00 UTC");
  });

  // The hint is said only to a reader who lacks services:configure, and then no write control is offered.
  it("says what is missing, and offers no New key or Revoke, to a reader without services:configure", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["tunnels:read:own"] };
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    expect(
      await screen.findByText("Managing keys needs the services:configure permission."),
    ).toBeInTheDocument();
    const table = await screen.findByRole("table", { name: /api keys/i });
    await within(table).findByText("ci");
    expect(screen.queryByRole("button", { name: "New key" })).toBeNull();
    expect(within(table).queryByRole("button", { name: /revoke key/i })).toBeNull();
    expect(screen.queryByText(/admin/i)).toBeNull();
  });

  it("offers New key and Revoke to a custom role holding services:configure:own", async () => {
    db.me = { ...db.me, role: "operator", permissions: ["services:configure:own"] };
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    expect(await screen.findByRole("button", { name: "New key" })).toBeInTheDocument();
    const table = await screen.findByRole("table", { name: /api keys/i });
    expect(await within(table).findByRole("button", { name: /revoke key ci/i })).toBeInTheDocument();
    expect(screen.queryByText("Managing keys needs the services:configure permission.")).toBeNull();
  });

  it("hides the permission hint from a reader who holds the permission", async () => {
    // db.me defaults to admin — assert the hint is absent.
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    await screen.findByRole("table", { name: /api keys/i });
    expect(
      screen.queryByText("Managing keys needs the services:configure permission."),
    ).toBeNull();
  });

  it("creates a key and reveals the plaintext exactly once", async () => {
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    await screen.findByRole("table", { name: /api keys/i });
    await userEvent.click(await screen.findByRole("button", { name: "New key" }));
    await userEvent.type(screen.getByLabelText(/key name/i), "deploy");
    await userEvent.click(screen.getByRole("button", { name: /^create$/i }));

    const reveal = await screen.findByText(/store it now — you won't see it again\./i);
    expect(reveal).toBeInTheDocument();
    const keyText = await screen.findByText(/^buk_mock_/);
    expect(keyText).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /copy api key/i })).toBeInTheDocument();

    // Dismiss the reveal — the plaintext must no longer be retrievable.
    await userEvent.click(screen.getByRole("button", { name: /done/i }));
    await waitFor(() => expect(screen.queryByText(/^buk_mock_/)).toBeNull());
  });

  it("does not render a redundant 'API keys' heading (the tab already names the panel) (L-19)", async () => {
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    await screen.findByRole("table", { name: /api keys/i });
    expect(screen.queryByRole("heading", { name: /^api keys$/i })).toBeNull();
  });

  it("shows a centred empty row when the service has no keys, not while loading (C6)", async () => {
    server.use(http.get("/api/v1/services/:id/api-keys", async () => {
      await delay(100);
      return HttpResponse.json([]);
    }));
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    await screen.findByRole("table", { name: /api keys/i });
    expect(screen.queryByText("No keys yet.")).toBeNull();
    const cell = (await screen.findByText("No keys yet.")).closest("td")!;
    expect(cell).toHaveClass("table-empty");
    expect(cell.getAttribute("colspan")).toBe("4");
  });

  it("intro: stands over the keys once they are listed, and not over a list that could not be read", async () => {
    const first = renderApp(<ApiKeysPanel serviceId={SVC} intro={<p>About these keys.</p>} />);
    // Not before the answer: it would name keys that may never be shown.
    expect(screen.queryByText("About these keys.")).toBeNull();
    const intro = await screen.findByText("About these keys.");
    const table = screen.getByRole("table", { name: /api keys/i });
    expect(intro.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    first.unmount();

    server.use(http.get("/api/v1/services/:id/api-keys", () => HttpResponse.json({ error: "forbidden" }, { status: 403 })));
    const { qc } = renderApp(<ApiKeysPanel serviceId={SVC} intro={<p>About these keys.</p>} />);
    await waitFor(() => expect(qc.getQueryState(["service-api-keys", SVC])?.status).toBe("error"));
    expect(screen.queryByText("About these keys.")).toBeNull();
  });

  it("revokeOnly: lists the keys with Revoke and offers no new one", async () => {
    const onRevoked = vi.fn();
    renderApp(<ApiKeysPanel serviceId={SVC} revokeOnly onRevoked={onRevoked} />);
    const table = await screen.findByRole("table", { name: /api keys/i });
    expect(await within(table).findByText("ci")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New key" })).toBeNull();
    await userEvent.click(within(table).getByRole("button", { name: /revoke key prod/i }));
    expect(onRevoked).not.toHaveBeenCalled();
    await userEvent.click(await screen.findByRole("button", { name: /^revoke$/i }));
    await waitFor(() => expect(within(table).queryByText("prod")).toBeNull());
    expect(onRevoked).toHaveBeenCalledTimes(1);
    expect(db.serviceApiKeys[SVC]!.map((k) => k.name)).toEqual(["ci"]);
  });

  it("revokes a key after confirmation", async () => {
    renderApp(<ApiKeysPanel serviceId={SVC} />);
    const table = await screen.findByRole("table", { name: /api keys/i });
    await userEvent.click(await within(table).findByRole("button", { name: /revoke key prod/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^revoke$/i }));
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: /api keys/i })).queryByText("prod")).toBeNull(),
    );
  });
});
