import { describe, it, expect, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import type { KnownClient } from "@/lib/contract";
import { clientsOnline, useKnownClients } from "./useKnownClients";

const row = (token_id: string, token_name: string, status: KnownClient["status"]): KnownClient => ({
  token_id, token_name, user_id: "u1", status, last_seen: null,
  os: "", arch: "", client_version: "", remote_addr: "", served: [], sessions: [],
});

describe("clientsOnline", () => {
  it("counts the rows that are online", () => {
    expect(clientsOnline([])).toBe(0);
    expect(clientsOnline([row("t1", "a", "online"), row("t2", "b", "offline"), row("t3", "c", "never_connected")])).toBe(1);
  });
  it("two tokens of one user with the same name are two clients", () => {
    expect(clientsOnline([row("t1", "laptop", "online"), row("t2", "laptop", "online")])).toBe(2);
  });
  it("so are two live sessions whose tokens were revoked", () => {
    expect(clientsOnline([row("", "laptop", "online"), row("", "laptop", "online")])).toBe(2);
  });
});

function Probe({ enabled, name }: { enabled?: boolean; name: string }) {
  const { rows, isSuccess, error } = useKnownClients({ enabled });
  return (
    <output data-testid={name}>
      {error ? "error" : !isSuccess ? "none" : JSON.stringify((rows ?? []).map((c) => [c.token_name, c.status, c.sessions.length]))}
    </output>
  );
}

describe("useKnownClients", () => {
  afterEach(() => resetDb());

  const requests = () => {
    const asked: string[] = [];
    server.events.on("request:start", ({ request }) => asked.push(new URL(request.url).pathname));
    return asked;
  };

  it("returns the relay's rows, and two readers share one request", async () => {
    const asked = requests();
    try {
      renderApp(<><Probe name="a" /><Probe name="b" /></>);
      const want = JSON.stringify([["office-box-1", "online", 1], ["koehn-laptop", "offline", 0], ["build-server", "never_connected", 0]]);
      await waitFor(() => expect(screen.getByTestId("a")).toHaveTextContent(want));
      expect(screen.getByTestId("b")).toHaveTextContent(want);
      expect(asked.filter((p) => p === "/api/v1/clients/known")).toHaveLength(1);
      expect(asked).not.toContain("/api/v1/clients");
    } finally {
      server.events.removeAllListeners();
    }
  });

  it("on a relay without the list, every live session is a row", async () => {
    db.clients.push({ ...db.clients[0]!, session_id: "sess_second" });
    server.use(http.get("/api/v1/clients/known", () => new HttpResponse(null, { status: 404 })));
    renderApp(<Probe name="a" />);
    await waitFor(() => expect(screen.getByTestId("a")).toHaveTextContent(JSON.stringify([["office-box-1", "online", 1], ["office-box-1", "online", 1]])));
  });

  it("a refusal is an error, and the live sessions are not asked for instead", async () => {
    db.me.role = "user";
    const asked = requests();
    try {
      renderApp(<Probe name="a" />);
      await waitFor(() => expect(screen.getByTestId("a")).toHaveTextContent("error"));
      expect(asked).not.toContain("/api/v1/clients");
    } finally {
      server.events.removeAllListeners();
    }
  });

  it("asks nothing while it is disabled", async () => {
    const asked = requests();
    try {
      renderApp(<><Probe name="a" enabled={false} /><Probe name="me" enabled={false} /></>);
      await new Promise((r) => setTimeout(r, 50));
      expect(screen.getByTestId("a")).toHaveTextContent("none");
      expect(asked.filter((p) => p.startsWith("/api/v1/clients"))).toEqual([]);
    } finally {
      server.events.removeAllListeners();
    }
  });
});
