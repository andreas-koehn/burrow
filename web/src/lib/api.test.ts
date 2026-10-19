import { describe, it, expect, vi, beforeEach } from "vitest";
import { apiFetch, downloadFile, ApiError, setReauthHandler, REAUTH_CANCELLED } from "./api";

beforeEach(() => {
  vi.restoreAllMocks();
  setReauthHandler(null);
  // Reset document.cookie between tests.
  // In jsdom, individual cookie pairs can be cleared by setting them expired.
  if (typeof document !== "undefined") {
    document.cookie = "burrow_csrf=; Max-Age=-1; path=/";
  }
});

describe("apiFetch", () => {
  it("returns parsed JSON on 200", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ ok: 1 }), { status: 200, headers: { "content-type": "application/json" } }) as any);
    expect(await apiFetch("/me")).toEqual({ ok: 1 });
  });
  it("throws ApiError with status on non-2xx", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ error: "bad" }), { status: 400 }) as any);
    await expect(apiFetch("/x")).rejects.toMatchObject({ status: 400 } as ApiError);
  });
  it("sends credentials and /api/v1 prefix", async () => {
    const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }) as any);
    await apiFetch("/tunnels");
    expect(f).toHaveBeenCalledWith("/api/v1/tunnels", expect.objectContaining({ credentials: "include" }));
  });
  it("returns null for an empty 204 body", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 204 }) as any);
    // Set the CSRF cookie so the POST is not blocked in the real server, but here
    // we only test the apiFetch client-side behaviour.
    if (typeof document !== "undefined") {
      document.cookie = "burrow_csrf=testtoken; path=/";
    }
    expect(await apiFetch("/auth/logout", { method: "POST" })).toBeNull();
  });
  it("throws ApiError(401) on 401 without mutating window.location", async () => {
    const initialHref = typeof globalThis.location !== "undefined" ? globalThis.location.href : undefined;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 401 }) as any);
    await expect(apiFetch("/me")).rejects.toMatchObject({ status: 401 });
    // apiFetch must NOT do a full-page redirect — location must be unchanged
    if (typeof globalThis.location !== "undefined") {
      expect(globalThis.location.href).toBe(initialHref);
    }
  });

  describe("CSRF header", () => {
    it("sends X-CSRF-Token on POST when burrow_csrf cookie is present", async () => {
      if (typeof document !== "undefined") {
        document.cookie = "burrow_csrf=my-csrf-token; path=/";
      }
      const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }) as any);
      await apiFetch("/tokens", { method: "POST" });
      const [, opts] = f.mock.calls[0] as [string, RequestInit];
      const headers = opts.headers as Record<string, string>;
      expect(headers["X-CSRF-Token"]).toBe("my-csrf-token");
    });

    it("sends X-CSRF-Token on DELETE when burrow_csrf cookie is present", async () => {
      if (typeof document !== "undefined") {
        document.cookie = "burrow_csrf=del-csrf-token; path=/";
      }
      const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 204 }) as any);
      await apiFetch("/tokens/t1", { method: "DELETE" });
      const [, opts] = f.mock.calls[0] as [string, RequestInit];
      const headers = opts.headers as Record<string, string>;
      expect(headers["X-CSRF-Token"]).toBe("del-csrf-token");
    });

    it("does NOT send X-CSRF-Token on GET", async () => {
      if (typeof document !== "undefined") {
        document.cookie = "burrow_csrf=should-not-appear; path=/";
      }
      const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }) as any);
      await apiFetch("/tunnels"); // default method is GET
      const [, opts] = f.mock.calls[0] as [string, RequestInit];
      const headers = opts.headers as Record<string, string>;
      expect(headers["X-CSRF-Token"]).toBeUndefined();
    });

    it("does NOT send X-CSRF-Token on HEAD", async () => {
      if (typeof document !== "undefined") {
        document.cookie = "burrow_csrf=should-not-appear; path=/";
      }
      const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 200 }) as any);
      await apiFetch("/me", { method: "HEAD" });
      const [, opts] = f.mock.calls[0] as [string, RequestInit];
      const headers = opts.headers as Record<string, string>;
      expect(headers["X-CSRF-Token"]).toBeUndefined();
    });

    it("sends no X-CSRF-Token header when cookie is absent (pre-login)", async () => {
      // Cookie cleared in beforeEach; POST /auth/login is exempt on the server.
      const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(
        new Response(JSON.stringify({ id: "u1" }), { status: 200 }) as any);
      await apiFetch("/auth/login", { method: "POST" });
      const [, opts] = f.mock.calls[0] as [string, RequestInit];
      const headers = opts.headers as Record<string, string>;
      // Header should be absent (or empty string filtered out); either way not
      // a truthy value — the server exempts login anyway.
      expect(headers["X-CSRF-Token"] || "").toBe("");
    });

    it("caller-supplied headers are merged and X-CSRF-Token is always present on POST", async () => {
      // This test proves Fix-1: before the fix, ...opts spread after the headers
      // object would replace the entire headers key (including X-CSRF-Token) with
      // opts.headers. After the fix, opts.headers is destructured and merged INTO
      // the final headers object, so X-CSRF-Token is always present on POST.
      if (typeof document !== "undefined") {
        document.cookie = "burrow_csrf=secure-token; path=/";
      }
      const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }) as any);
      await apiFetch("/tokens", {
        method: "POST",
        // Caller passes an extra header. Previously, supplying opts.headers
        // caused ...opts to overwrite the headers key, clobbering X-CSRF-Token.
        headers: { "X-Custom": "my-value" },
      });
      const [, callOpts] = f.mock.calls[0] as [string, RequestInit];
      const headers = callOpts.headers as Record<string, string>;
      // CSRF token must still be present — not clobbered by caller opts.headers.
      expect(headers["X-CSRF-Token"]).toBe("secure-token");
      // Default Content-Type must still be present.
      expect(headers["Content-Type"]).toBe("application/json");
      // Caller's extra header is merged in alongside the defaults.
      expect(headers["X-Custom"]).toBe("my-value");
    });
  });
});

describe("reauth_required", () => {
  const refused = () => new Response(JSON.stringify({ error: "reauth_required" }), { status: 403 });
  const ok = (body: unknown = { done: 1 }) => new Response(JSON.stringify(body), { status: 200 });

  it("asks the handler, then repeats the call once with the same request", async () => {
    const f = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(refused() as any).mockResolvedValueOnce(ok() as any);
    const handler = vi.fn().mockResolvedValue(undefined);
    setReauthHandler(handler);
    const out = await apiFetch("/tokens", { method: "POST", body: JSON.stringify({ name: "x" }) });
    expect(out).toEqual({ done: 1 });
    expect(handler).toHaveBeenCalledTimes(1);
    expect(f).toHaveBeenCalledTimes(2);
    const [[, a], [, b]] = f.mock.calls as [string, RequestInit][];
    expect(b.body).toBe(a.body);
    expect(b.method).toBe("POST");
  });

  it("rejects the original call when the handler gives up, and does not repeat it", async () => {
    const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(refused() as any);
    setReauthHandler(() => Promise.reject(new ApiError(403, "Password confirmation cancelled.", REAUTH_CANCELLED)));
    await expect(apiFetch("/tokens", { method: "POST" })).rejects.toMatchObject({ status: 403, code: REAUTH_CANCELLED });
    expect(f).toHaveBeenCalledTimes(1);
  });

  it("asks only once per call: a second refusal after the repeat is an ordinary error", async () => {
    const f = vi.spyOn(globalThis, "fetch").mockResolvedValue(refused() as any);
    const handler = vi.fn().mockResolvedValue(undefined);
    setReauthHandler(handler);
    await expect(apiFetch("/tokens", { method: "POST" })).rejects.toMatchObject({ status: 403, message: "reauth_required" });
    expect(handler).toHaveBeenCalledTimes(1);
    expect(f).toHaveBeenCalledTimes(2);
  });

  it("is an ordinary 403 without a handler", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(refused() as any);
    await expect(apiFetch("/tokens", { method: "POST" })).rejects.toMatchObject({ status: 403, message: "reauth_required" });
  });

  it("leaves other 403s alone", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ error: "users:manage required" }), { status: 403 }) as any);
    const handler = vi.fn().mockResolvedValue(undefined);
    setReauthHandler(handler);
    await expect(apiFetch("/users", { method: "POST" })).rejects.toMatchObject({ status: 403, message: "users:manage required" });
    expect(handler).not.toHaveBeenCalled();
  });

  it("never sends the confirmation call itself through the dialog", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(refused() as any);
    const handler = vi.fn().mockResolvedValue(undefined);
    setReauthHandler(handler);
    await expect(apiFetch("/auth/reauth", { method: "POST" })).rejects.toMatchObject({ status: 403 });
    expect(handler).not.toHaveBeenCalled();
  });

  it("serves a download the same way", async () => {
    const f = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(refused() as any)
      .mockResolvedValueOnce(new Response("data", { status: 200 }) as any);
    const handler = vi.fn().mockResolvedValue(undefined);
    setReauthHandler(handler);
    await downloadFile("/backups/b1/download", "b.tar.gz");
    expect(handler).toHaveBeenCalledTimes(1);
    expect(f).toHaveBeenCalledTimes(2);
  });
});
