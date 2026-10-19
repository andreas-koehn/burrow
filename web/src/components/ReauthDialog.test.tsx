import { describe, it, expect, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { ReauthDialog } from "@/components/ReauthDialog";
import { apiFetch, ApiError, REAUTH_CANCELLED, setReauthHandler } from "@/lib/api";
import { server } from "@/mocks/server";
import { db } from "@/mocks/db";
import { setCsrfCookie } from "@/mocks/test-utils";

// A guarded route of the relay: refuses with reauth_required until the
// password is confirmed (the mock's POST /auth/reauth), then answers.
function guarded() {
  const seen: string[] = [];
  server.use(http.post("/api/v1/tokens", () => {
    seen.push("POST /tokens");
    return db.reauthConfirmed ? HttpResponse.json({ ok: true }) : HttpResponse.json({ error: "reauth_required" }, { status: 403 });
  }));
  return seen;
}

const dialog = () => screen.findByRole("dialog", { name: "Confirm your password" });
const field = () => screen.getByLabelText("Your password");

afterEach(() => setReauthHandler(null));

describe("ReauthDialog", () => {
  it("opens on reauth_required, explains why, confirms and lets the call through", async () => {
    setCsrfCookie();
    const seen = guarded();
    render(<ReauthDialog />);
    const call = apiFetch<{ ok: boolean }>("/tokens", { method: "POST" });
    const d = await dialog();
    expect(d).toHaveAttribute("aria-modal", "true");
    expect(d).toHaveTextContent(/no separate services domain/);
    expect(field()).toHaveAttribute("type", "password");
    expect(screen.getByRole("button", { name: "Confirm" })).toBeDisabled();
    await userEvent.type(field(), "password123");
    await userEvent.click(screen.getByRole("button", { name: "Confirm" }));
    await expect(call).resolves.toEqual({ ok: true });
    expect(seen).toEqual(["POST /tokens", "POST /tokens"]);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps the dialog and says so on a wrong password, and Enter submits", async () => {
    setCsrfCookie();
    guarded();
    render(<ReauthDialog />);
    const call = apiFetch("/tokens", { method: "POST" });
    await dialog();
    await userEvent.type(field(), "nope{Enter}");
    expect(await screen.findByText("The password is not correct.")).toBeInTheDocument();
    expect(field()).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.clear(field());
    await userEvent.type(field(), "password123{Enter}");
    await expect(call).resolves.toEqual({ ok: true });
  });

  it("rejects the original call with a clear error when cancelled (button, Escape)", async () => {
    setCsrfCookie();
    const seen = guarded();
    render(<ReauthDialog />);
    for (const how of ["button", "escape"] as const) {
      const call = apiFetch("/tokens", { method: "POST" });
      const caught = call.catch((e: unknown) => e);
      await dialog();
      if (how === "button") await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
      else await userEvent.keyboard("{Escape}");
      const e = await caught;
      expect(e).toBeInstanceOf(ApiError);
      expect(e).toMatchObject({ status: 403, code: REAUTH_CANCELLED, message: "Password confirmation cancelled." });
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    }
    // Each call was tried once and not repeated.
    expect(seen).toEqual(["POST /tokens", "POST /tokens"]);
  });

  it("shares one dialog between calls that arrive together and repeats each once", async () => {
    setCsrfCookie();
    const seen = guarded();
    render(<ReauthDialog />);
    const a = apiFetch("/tokens", { method: "POST" });
    const b = apiFetch("/tokens", { method: "POST" });
    await dialog();
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    await userEvent.type(field(), "password123{Enter}");
    await expect(Promise.all([a, b])).resolves.toEqual([{ ok: true }, { ok: true }]);
    expect(seen).toHaveLength(4);
  });

  it("traps Tab inside the dialog", async () => {
    setCsrfCookie();
    guarded();
    render(<><button type="button">behind</button><ReauthDialog /></>);
    void apiFetch("/tokens", { method: "POST" }).catch(() => undefined);
    const d = await dialog();
    await waitFor(() => expect(d).toContainElement(document.activeElement as HTMLElement));
    for (let i = 0; i < 6; i++) {
      await userEvent.tab();
      expect(d).toContainElement(document.activeElement as HTMLElement);
    }
  });

  it("says when the attempts are used up", async () => {
    setCsrfCookie();
    guarded();
    server.use(http.post("/api/v1/auth/reauth", () => HttpResponse.json({ error: "too many login attempts" }, { status: 429 })));
    render(<ReauthDialog />);
    void apiFetch("/tokens", { method: "POST" }).catch(() => undefined);
    await dialog();
    await userEvent.type(field(), "password123{Enter}");
    expect(await screen.findByText("Too many attempts. Wait a minute, then try again.")).toBeInTheDocument();
  });
});
