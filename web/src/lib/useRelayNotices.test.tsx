import { describe, it, expect, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { db, resetDb } from "@/mocks/db";
import { EMAIL_NOT_CONFIGURED } from "@/lib/copy";
import { useAuth } from "@/auth/useAuth";
import { useRelayNotices } from "./useRelayNotices";

function Probe() {
  const { user } = useAuth();
  const notices = useRelayNotices();
  return (
    <>
      <output data-testid="role">{user?.role ?? ""}</output>
      <output data-testid="notices">{JSON.stringify(notices)}</output>
    </>
  );
}

const notices = () => JSON.parse(screen.getByTestId("notices").textContent ?? "null") as unknown;
const signedInAs = (role: string) => waitFor(() => expect(screen.getByTestId("role")).toHaveTextContent(role));

/** Counts the settings requests and answers them with `respond`. */
function watchSettings(respond: () => Response) {
  const seen = { count: 0 };
  server.use(http.get("/api/v1/settings", () => { seen.count += 1; return respond(); }));
  return seen;
}

describe("useRelayNotices", () => {
  afterEach(() => resetDb());

  it("admin, SMTP not configured: one notice that leads to the email settings", async () => {
    renderApp(<Probe />);
    await waitFor(() => expect(notices()).toEqual([
      { id: "email", message: EMAIL_NOT_CONFIGURED, action: { label: "Set up email", to: "/settings/email" } },
    ]));
  });

  it("admin, SMTP configured: no notice", async () => {
    const seen = watchSettings(() => HttpResponse.json({ "smtp.host": "smtp.example.com" }));
    renderApp(<Probe />);
    await waitFor(() => expect(seen.count).toBe(1));
    await signedInAs("admin");
    expect(notices()).toEqual([]);
  });

  it("non-admin: no notice, and the settings endpoint is not requested", async () => {
    db.me = { ...db.me, role: "user" };
    const seen = watchSettings(() => HttpResponse.json({}));
    renderApp(<Probe />);
    await signedInAs("user");
    // Give a wrongly enabled query the time to fire.
    await new Promise((r) => setTimeout(r, 50));
    expect(seen.count).toBe(0);
    expect(notices()).toEqual([]);
  });

  it("the settings request failing: no notice (never shown on a guess)", async () => {
    const seen = watchSettings(() => HttpResponse.json({ error: "boom" }, { status: 500 }));
    renderApp(<Probe />);
    await waitFor(() => expect(seen.count).toBe(1));
    await new Promise((r) => setTimeout(r, 50));
    expect(notices()).toEqual([]);
  });
});
